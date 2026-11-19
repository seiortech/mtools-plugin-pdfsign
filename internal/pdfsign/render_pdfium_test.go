//go:build !darwin || pdfium

package pdfsign

import (
	"context"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"testing"
)

const vectorBoxPDF = `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R /Resources <<>> >>
endobj
4 0 obj
<< /Length 44 >>
stream
0 0 0 rg
20 20 160 160 re
f
endstream
endobj
xref
0 5
0000000000 65535 f 
0000000009 00000 n 
0000000058 00000 n 
0000000115 00000 n 
0000000210 00000 n 
trailer
<< /Size 5 /Root 1 0 R >>
startxref
304
%%EOF`

func TestPDFiumRendererMissingAndInvalid(t *testing.T) {
	// Ensure pdftoppm is not found in PATH so missing WASM returns ErrPDFiumMissing.
	t.Setenv("PATH", t.TempDir())

	tmpDir := t.TempDir()
	pdfPath := filepath.Join(tmpDir, "test.pdf")
	if err := os.WriteFile(pdfPath, []byte(vectorBoxPDF), 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	r := NewPDFiumRenderer()
	defer func() { _ = r.Close() }()

	_, err := r.RenderPage(context.Background(), pdfPath, 1, 1.0)
	if !errors.Is(err, ErrPDFiumMissing) {
		t.Fatalf("expected ErrPDFiumMissing when unconfigured, got %v", err)
	}

	badWASM := filepath.Join(tmpDir, "bad.wasm")
	if err := os.WriteFile(badWASM, []byte("not-a-wasm"), 0o644); err != nil {
		t.Fatalf("write bad wasm: %v", err)
	}
	r.Configure(RendererConfig{WASMPath: badWASM, CacheDir: filepath.Join(tmpDir, "cache")})

	_, err = r.RenderPage(context.Background(), pdfPath, 1, 1.0)
	if !errors.Is(err, ErrPDFiumInvalid) {
		t.Fatalf("expected ErrPDFiumInvalid for bad wasm, got %v", err)
	}
}

func TestPDFiumRendererRealWASMSmoke(t *testing.T) {
	tmpDir := t.TempDir()
	pdfPath := filepath.Join(tmpDir, "box.pdf")
	if err := os.WriteFile(pdfPath, []byte(vectorBoxPDF), 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	r := NewPDFiumRenderer()
	configureTestRenderer(t, r)

	img, err := r.RenderPage(context.Background(), pdfPath, 1, 1.0)
	if err != nil {
		t.Fatalf("RenderPage with pdfium.wasm failed: %v", err)
	}
	b := img.Bounds()
	// 200x200 pt * 2x HiDPI = 400x400 px
	if b.Dx() != 400 || b.Dy() != 400 {
		t.Fatalf("expected 400x400 HiDPI image, got %dx%d", b.Dx(), b.Dy())
	}

	// Center pixel (200,200) is inside the black filled rectangle (20..180 pt).
	c := color.RGBAModel.Convert(img.At(200, 200)).(color.RGBA)
	if c.R > 30 || c.G > 30 || c.B > 30 {
		t.Fatalf("expected dark rendered pixel at center, got %+v", c)
	}

	cnt, err := r.PageCount(context.Background(), pdfPath)
	if err != nil || cnt != 1 {
		t.Fatalf("PageCount = (%d, %v), want (1, nil)", cnt, err)
	}
	wPt, hPt, err := r.PageDimensions(context.Background(), pdfPath, 1)
	if err != nil || wPt != 200 || hPt != 200 {
		t.Fatalf("PageDimensions = (%v, %v, %v), want (200, 200, nil)", wPt, hPt, err)
	}
}

func TestPDFiumRendererCheckEngine(t *testing.T) {
	r := NewPDFiumRenderer()
	if err := r.CheckEngine(); err != nil && err != ErrPDFiumMissing {
		t.Fatalf("CheckEngine unconfigured = %v, want ErrPDFiumMissing", err)
	}

	badPath := filepath.Join(t.TempDir(), "bad.wasm")
	if err := os.WriteFile(badPath, []byte("not-wasm"), 0o644); err != nil {
		t.Fatalf("write bad wasm: %v", err)
	}
	r.Configure(RendererConfig{WASMPath: badPath})
	if _, poolErr := r.ensurePool(); poolErr == nil {
		t.Fatalf("expected ensurePool error for invalid wasm")
	}
}
