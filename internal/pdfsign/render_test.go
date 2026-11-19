package pdfsign

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func findTestPDFiumWASM(t *testing.T) string {
	t.Helper()
	if p := strings.TrimSpace(os.Getenv("MTOOLS_TEST_PDFIUM_WASM")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if out, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
		modCache := strings.TrimSpace(string(out))
		if modCache != "" {
			candidate := filepath.Join(modCache, "github.com", "klippa-app", "go-pdfium@v1.21.4", "internal", "pdfium_wasm", "pdfium.wasm")
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return ""
}

func configureTestRenderer(t *testing.T, r PageRenderer) {
	t.Helper()
	cr, ok := r.(ConfigurableRenderer)
	if !ok {
		return
	}
	wasmPath := findTestPDFiumWASM(t)
	if wasmPath == "" {
		t.Skip("pdfium.wasm not found in MTOOLS_TEST_PDFIUM_WASM or GOMODCACHE")
	}
	cacheDir := filepath.Join(os.TempDir(), "mtools-test-pdfium-cache")
	cr.Configure(RendererConfig{
		WASMPath: wasmPath,
		CacheDir: cacheDir,
	})
	t.Cleanup(func() {
		_ = cr.Close()
	})
}

const minimalPDF = `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 300] /Resources <<>> >>
endobj
xref
0 4
0000000000 65535 f 
0000000009 00000 n 
0000000058 00000 n 
0000000115 00000 n 
trailer
<< /Size 4 /Root 1 0 R >>
startxref
198
%%EOF`

func TestRenderer(t *testing.T) {
	tmpDir := t.TempDir()
	pdfPath := filepath.Join(tmpDir, "test.pdf")
	if err := os.WriteFile(pdfPath, []byte(minimalPDF), 0644); err != nil {
		t.Fatalf("failed to write test pdf: %v", err)
	}

	renderer := DefaultRenderer()
	configureTestRenderer(t, renderer)
	ctx := context.Background()

	count, err := renderer.PageCount(ctx, pdfPath)
	if err != nil {
		t.Fatalf("PageCount failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 page, got %d", count)
	}

	w, h, err := renderer.PageDimensions(ctx, pdfPath, 1)
	if err != nil {
		t.Fatalf("PageDimensions failed: %v", err)
	}
	if int(w) != 200 || int(h) != 300 {
		t.Errorf("expected 200x300, got %vx%v", w, h)
	}

	img, err := renderer.RenderPage(ctx, pdfPath, 1, 1.0)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}
	// Under macOS Darwin CoreGraphics renderer, 2.0x HiDPI supersampling produces 400x600.
	// Under FallbackRenderer (or non-darwin platforms), it produces 200x300.
	wImg := img.Bounds().Dx()
	hImg := img.Bounds().Dy()
	if (wImg != 200 || hImg != 300) && (wImg != 400 || hImg != 600) {
		t.Errorf("expected image bounds 200x300 or 400x600 (HiDPI), got %vx%v", wImg, hImg)
	}

	// Test LRU cache
	cache := NewPageBitmapCache(2)
	cache.Put(pdfPath, 1, 1.0, img)
	cached, ok := cache.Get(pdfPath, 1, 1.0)
	if !ok || cached == nil {
		t.Fatalf("expected cached image")
	}

	cache.ClearForPath(pdfPath)
	_, ok = cache.Get(pdfPath, 1, 1.0)
	if ok {
		t.Fatalf("expected cache miss after ClearForPath")
	}

	// Test FallbackRenderer directly
	fallback := &FallbackRenderer{}
	fbCount, err := fallback.PageCount(ctx, pdfPath)
	if err != nil {
		t.Fatalf("Fallback PageCount failed: %v", err)
	}
	if fbCount != 1 {
		t.Errorf("expected 1 page, got %d", fbCount)
	}
	fbW, fbH, err := fallback.PageDimensions(ctx, pdfPath, 1)
	if err != nil {
		t.Fatalf("Fallback PageDimensions failed: %v", err)
	}
	if int(fbW) != 200 || int(fbH) != 300 {
		t.Errorf("expected 200x300, got %vx%v", fbW, fbH)
	}
	fbImg, err := fallback.RenderPage(ctx, pdfPath, 1, 1.0)
	if _, lookErr := exec.LookPath("pdftoppm"); lookErr == nil {
		if err != nil {
			t.Fatalf("Fallback RenderPage with pdftoppm failed: %v", err)
		}
		if fbImg.Bounds().Dx() <= 0 || fbImg.Bounds().Dy() <= 0 {
			t.Errorf("expected positive fallback image dimensions, got %vx%v", fbImg.Bounds().Dx(), fbImg.Bounds().Dy())
		}
	} else if !errors.Is(err, ErrNoPreviewEngine) {
		t.Fatalf("Fallback RenderPage = %v, want ErrNoPreviewEngine", err)
	}
}

func TestRendererOrientationAndHiDPI(t *testing.T) {
	if runtime.GOOS != "darwin" {
		if _, err := exec.LookPath("pdftoppm"); err != nil {
			t.Skip("skipping content orientation check: pdftoppm not installed and pure-Go fallback renders a blank placeholder")
		}
	}
	tmpDir := t.TempDir()
	pdfPath := filepath.Join(tmpDir, "top_black.pdf")
	topBlackPDF := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 300] /Resources <<>> /Contents 4 0 R >>
endobj
4 0 obj
<< /Length 27 >>
stream
0 0 0 rg
0 200 200 100 re
f
endstream
endobj
xref
0 5
0000000000 65535 f 
0000000009 00000 n 
0000000058 00000 n 
0000000115 00000 n 
0000000218 00000 n 
trailer
<< /Size 5 /Root 1 0 R >>
startxref
295
%%EOF`
	if err := os.WriteFile(pdfPath, []byte(topBlackPDF), 0644); err != nil {
		t.Fatalf("failed to write test pdf: %v", err)
	}

	renderer := DefaultRenderer()
	configureTestRenderer(t, renderer)
	ctx := context.Background()
	img, err := renderer.RenderPage(ctx, pdfPath, 1, 1.0)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}

	// At 1.0 scale (2.0x HiDPI on Darwin = 400x600, or 200x300 on fallback):
	// Top 1/3 of the page has a black bar; bottom 2/3 is white.
	// Sample near the top and near the bottom:
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()
	topColor := img.At(w/2, h/6)
	bottomColor := img.At(w/2, 5*h/6)

	tr, tg, tb, _ := topColor.RGBA()
	br, bg, bb, _ := bottomColor.RGBA()

	// Top should be darker than bottom (black bar at top)
	topLuma := 0.299*float64(tr) + 0.587*float64(tg) + 0.114*float64(tb)
	bottomLuma := 0.299*float64(br) + 0.587*float64(bg) + 0.114*float64(bb)

	if topLuma >= bottomLuma {
		t.Fatalf("expected top of page to be dark and bottom to be light (page is upside-down!): topLuma=%v, bottomLuma=%v", topLuma, bottomLuma)
	}
}
