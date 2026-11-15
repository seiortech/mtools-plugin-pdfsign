package pdfsign

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"os/exec"
	"strconv"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// FallbackRenderer provides PDF dimension parsing and image rendering on non-macOS or headless platforms.
type FallbackRenderer struct{}

func (r *FallbackRenderer) PageCount(ctx context.Context, pdfPath string) (int, error) {
	f, err := os.Open(pdfPath)
	if err != nil {
		return 0, fmt.Errorf("open pdf %s: %w", pdfPath, err)
	}
	defer f.Close()
	return api.PageCount(ctx, f, model.NewStatelessConfiguration())
}

func (r *FallbackRenderer) PageDimensions(ctx context.Context, pdfPath string, pageNum int) (float64, float64, error) {
	f, err := os.Open(pdfPath)
	if err != nil {
		return 0, 0, fmt.Errorf("open pdf %s: %w", pdfPath, err)
	}
	defer f.Close()
	dims, err := api.PageDims(ctx, f, model.NewStatelessConfiguration())
	if err != nil {
		return 0, 0, fmt.Errorf("read pdf dimensions %s: %w", pdfPath, err)
	}
	if pageNum < 1 || pageNum > len(dims) {
		return 0, 0, ErrPageOutOfRange
	}
	d := dims[pageNum-1]
	return d.Width, d.Height, nil
}

func (r *FallbackRenderer) RenderPage(ctx context.Context, pdfPath string, pageNum int, scale float64) (image.Image, error) {
	if scale <= 0 {
		scale = 1.0
	}

	// 1. Check if pdftoppm (poppler) is available on the system
	if pdftoppmPath, err := exec.LookPath("pdftoppm"); err == nil {
		dpi := int(math.Round(72.0 * scale))
		cmd := exec.CommandContext(ctx, pdftoppmPath,
			"-png",
			"-f", strconv.Itoa(pageNum),
			"-l", strconv.Itoa(pageNum),
			"-r", strconv.Itoa(dpi),
			pdfPath,
			"-",
		)
		setCmdHideWindow(cmd)
		var out bytes.Buffer
		cmd.Stdout = &out
		// pdftoppm writes to stdout when "-" output prefix is provided
		if err := cmd.Run(); err == nil {
			if img, err := png.Decode(&out); err == nil {
				return img, nil
			}
		}
	}

	// 2. Pure Go fallback rendering
	wPt, hPt, err := r.PageDimensions(ctx, pdfPath, pageNum)
	if err != nil {
		return nil, err
	}

	w := int(math.Ceil(wPt * scale))
	h := int(math.Ceil(hPt * scale))
	if w <= 0 || h <= 0 {
		w, h = int(595*scale), int(842*scale) // Standard A4 points
	}

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Fill with solid white
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)

	// Draw subtle border around page
	borderColor := color.RGBA{R: 220, G: 220, B: 220, A: 255}
	for x := 0; x < w; x++ {
		img.Set(x, 0, borderColor)
		img.Set(x, h-1, borderColor)
	}
	for y := 0; y < h; y++ {
		img.Set(0, y, borderColor)
		img.Set(w-1, y, borderColor)
	}

	return img, nil
}

// Ensure FallbackRenderer is always compiled and tested
var _ PageRenderer = (*FallbackRenderer)(nil)
