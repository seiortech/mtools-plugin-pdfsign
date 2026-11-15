package pdfsign

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

const (
	pdfiumHiDPIFactor = 2.0
	pdfiumMaxDimPx    = 4096
)

var wasmMagic = []byte{0x00, 'a', 's', 'm'}

// PDFiumRenderer renders PDF pages on non-macOS platforms using an external pdfium.wasm module
// loaded into go-pdfium's wazero WebAssembly runtime.
type PDFiumRenderer struct {
	mu       sync.Mutex
	cfg      RendererConfig
	pool     pdfium.Pool
	poolPath string
	poolErr  error
	compDir  string
	fallback *FallbackRenderer
}

// NewPDFiumRenderer creates a new PDFiumRenderer.
func NewPDFiumRenderer() *PDFiumRenderer {
	return &PDFiumRenderer{
		fallback: &FallbackRenderer{},
	}
}

// Configure updates the external pdfium.wasm path and compilation cache directory.
// Changing the WASM path resets any existing pool so the next render reloads it.
func (r *PDFiumRenderer) Configure(cfg RendererConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cfg == r.cfg {
		return
	}
	r.cfg = cfg
	if r.pool != nil && cfg.WASMPath != r.poolPath {
		_ = r.pool.Close()
		r.pool = nil
		r.poolPath = ""
	}
	r.poolErr = nil
}

// Close releases the underlying PDFium pool and compilation cache resources.
func (r *PDFiumRenderer) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.pool != nil {
		err = r.pool.Close()
		r.pool = nil
		r.poolPath = ""
	}
	r.poolErr = nil
	return err
}

func (r *PDFiumRenderer) PageCount(ctx context.Context, pdfPath string) (int, error) {
	return r.fallback.PageCount(ctx, pdfPath)
}

func (r *PDFiumRenderer) PageDimensions(ctx context.Context, pdfPath string, pageNum int) (float64, float64, error) {
	return r.fallback.PageDimensions(ctx, pdfPath, pageNum)
}

func (r *PDFiumRenderer) ensurePool() (pdfium.Pool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.pool != nil {
		return r.pool, nil
	}
	if r.poolErr != nil {
		return nil, r.poolErr
	}

	wasmPath := r.cfg.WASMPath
	if wasmPath == "" {
		r.poolErr = ErrPDFiumMissing
		return nil, r.poolErr
	}

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		if os.IsNotExist(err) {
			r.poolErr = fmt.Errorf("%w: %s", ErrPDFiumMissing, wasmPath)
		} else {
			r.poolErr = fmt.Errorf("%w: read %s: %v", ErrPDFiumInvalid, wasmPath, err)
		}
		return nil, r.poolErr
	}
	if len(wasmBytes) < len(wasmMagic) || !bytes.Equal(wasmBytes[:len(wasmMagic)], wasmMagic) {
		r.poolErr = fmt.Errorf("%w: %s is not a valid WebAssembly module", ErrPDFiumInvalid, wasmPath)
		return nil, r.poolErr
	}

	cfg := webassembly.Config{
		MinIdle:  0,
		MaxIdle:  1,
		MaxTotal: 2,
		WASM:     wasmBytes,
	}
	if r.cfg.CacheDir != "" {
		if err := os.MkdirAll(r.cfg.CacheDir, 0o755); err == nil {
			if cache, cacheErr := wazero.NewCompilationCacheWithDir(r.cfg.CacheDir); cacheErr == nil {
				cfg.RuntimeConfig = wazero.NewRuntimeConfig().
					WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
					WithCompilationCache(cache)
				r.compDir = r.cfg.CacheDir
			}
		}
	}

	pool, err := webassembly.InitWithWASM(cfg)
	if err != nil {
		r.poolErr = fmt.Errorf("%w: %v", ErrPDFiumInvalid, err)
		return nil, r.poolErr
	}
	r.pool = pool
	r.poolPath = wasmPath
	return pool, nil
}

func (r *PDFiumRenderer) RenderPage(ctx context.Context, pdfPath string, pageNum int, scale float64) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scale <= 0 {
		scale = 1.0
	}

	pool, poolErr := r.ensurePool()
	if poolErr != nil {
		// On systems with poppler pdftoppm installed, allow it as a secondary fallback.
		if _, lookErr := exec.LookPath("pdftoppm"); lookErr == nil {
			return r.fallback.RenderPage(ctx, pdfPath, pageNum, scale)
		}
		return nil, poolErr
	}

	if pageNum < 1 {
		return nil, ErrPageOutOfRange
	}

	// Compute DPI with 2x HiDPI multiplier, capped to keep bitmap dimensions within pdfiumMaxDimPx.
	effectiveScale := scale * pdfiumHiDPIFactor
	if wPt, hPt, dimErr := r.fallback.PageDimensions(ctx, pdfPath, pageNum); dimErr == nil && wPt > 0 && hPt > 0 {
		maxPt := math.Max(wPt, hPt)
		if maxPt*effectiveScale > pdfiumMaxDimPx {
			effectiveScale = pdfiumMaxDimPx / maxPt
		}
	}
	dpi := int(math.Round(72.0 * effectiveScale))
	if dpi < 36 {
		dpi = 36
	}

	pdfBytes, err := os.ReadFile(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("read pdf %s: %w", pdfPath, err)
	}

	instance, err := pool.GetInstance(30 * time.Second)
	if err != nil {
		return nil, fmt.Errorf("acquire pdfium instance: %w", err)
	}
	defer instance.Close()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	doc, err := instance.OpenDocument(&requests.OpenDocument{File: &pdfBytes})
	if err != nil {
		return nil, fmt.Errorf("open pdf %s: %w", pdfPath, err)
	}
	defer func() {
		_, _ = instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
	}()

	resp, err := instance.RenderPageInDPI(&requests.RenderPageInDPI{
		Page: requests.Page{
			ByIndex: &requests.PageByIndex{
				Document: doc.Document,
				Index:    pageNum - 1,
			},
		},
		DPI:        dpi,
		RenderForm: true,
		Document:   &doc.Document,
	})
	if err != nil {
		return nil, fmt.Errorf("render page %d: %w", pageNum, err)
	}
	defer resp.Cleanup()

	if resp.Result.RenderedImage == nil {
		return nil, fmt.Errorf("render page %d: empty image", pageNum)
	}

	// Copy pixels into a Go-owned RGBA image before resp.Cleanup() invalidates the WASM buffer.
	return cloneToRGBA(resp.Result.RenderedImage), nil
}

func cloneToRGBA(src image.Image) *image.RGBA {
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	return dst
}
