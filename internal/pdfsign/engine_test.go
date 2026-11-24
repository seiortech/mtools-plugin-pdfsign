package pdfsign

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func createSolidTestImage(w, h int, c color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestSignPDFSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	inPDF := filepath.Join(tmpDir, "input.pdf")
	outPDF := filepath.Join(tmpDir, "output.pdf")

	if err := os.WriteFile(inPDF, []byte(minimalPDF), 0644); err != nil {
		t.Fatalf("failed to write test pdf: %v", err)
	}

	sigImg := createSolidTestImage(100, 50, color.RGBA{R: 0, G: 0, B: 255, A: 255})

	req := SignRequest{
		InFile:  inPDF,
		OutFile: outPDF,
		Signatures: []PlacedSignature{
			{
				ID:       "sig-1",
				Page:     1,
				X:        20,
				Y:        30,
				Width:    50,
				Height:   25,
				Opacity:  0.9,
				Rotation: 0,
				InkMode:  InkOriginal,
				Image:    sigImg,
			},
		},
	}

	progressCalls := 0
	progress := func(done float64, msg string) {
		progressCalls++
	}

	ctx := context.Background()
	err := SignPDF(ctx, req, progress)
	if err != nil {
		t.Fatalf("SignPDF failed: %v", err)
	}

	if progressCalls == 0 {
		t.Errorf("expected progress calls, got 0")
	}

	stat, err := os.Stat(outPDF)
	if err != nil || stat.Size() == 0 {
		t.Fatalf("expected signed pdf file at %s", outPDF)
	}

	// Verify the signed PDF can be rendered and has 1 page
	renderer := DefaultRenderer()
	configureTestRenderer(t, renderer)
	count, err := renderer.PageCount(ctx, outPDF)
	if err != nil {
		t.Fatalf("render count on signed PDF failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 page, got %d", count)
	}
}

func TestSignPDFValidationErrors(t *testing.T) {
	ctx := context.Background()

	// Empty input
	err := SignPDF(ctx, SignRequest{}, nil)
	if err != ErrEmptyInput {
		t.Errorf("expected ErrEmptyInput, got %v", err)
	}

	// No signatures
	err = SignPDF(ctx, SignRequest{InFile: "a.pdf", OutFile: "b.pdf"}, nil)
	if err != ErrNoSignatures {
		t.Errorf("expected ErrNoSignatures, got %v", err)
	}

	// Invalid page
	tmpDir := t.TempDir()
	inPDF := filepath.Join(tmpDir, "input.pdf")
	outPDF := filepath.Join(tmpDir, "output.pdf")
	_ = os.WriteFile(inPDF, []byte(minimalPDF), 0644)

	err = SignPDF(ctx, SignRequest{
		InFile:  inPDF,
		OutFile: outPDF,
		Signatures: []PlacedSignature{
			{
				ID:     "s1",
				Page:   99, // out of range
				Width:  50,
				Height: 50,
				Image:  createSolidTestImage(10, 10, color.Black),
			},
		},
	}, nil)
	if err == nil {
		t.Errorf("expected error for invalid page, got nil")
	}
}

func TestSignPDFCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	inPDF := filepath.Join(tmpDir, "input.pdf")
	outPDF := filepath.Join(tmpDir, "output.pdf")
	_ = os.WriteFile(inPDF, []byte(minimalPDF), 0644)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := SignPDF(ctx, SignRequest{
		InFile:  inPDF,
		OutFile: outPDF,
		Signatures: []PlacedSignature{
			{
				ID:     "s1",
				Page:   1,
				Width:  50,
				Height: 50,
				Image:  createSolidTestImage(10, 10, color.Black),
			},
		},
	}, nil)

	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}

	// Verify destination file was not created
	if _, err := os.Stat(outPDF); !os.IsNotExist(err) {
		t.Errorf("expected outPDF to not exist after cancellation")
	}
}

func TestSignPDFWYSIWYGPositionSizeAndRotation(t *testing.T) {
	tmpDir := t.TempDir()
	inPDF := filepath.Join(tmpDir, "input.pdf")
	outPDF := filepath.Join(tmpDir, "output.pdf")

	if err := os.WriteFile(inPDF, []byte(minimalPDF), 0644); err != nil {
		t.Fatalf("failed to write test pdf: %v", err)
	}

	// 160x80 (2:1) solid blue signature
	sigImg := createSolidTestImage(160, 80, color.RGBA{R: 0, G: 0, B: 220, A: 255})

	// Case 1: unrotated 2:1 box at (30, 40) size 80x40 on 200x300 page.
	// Case 2: 90-deg rotated (becomes 1:2) inside a 100x60 box at (40, 120);
	// Contain fit must shrink to 30x60 centered at X=75, Y=120 without overflowing.
	req := SignRequest{
		InFile:  inPDF,
		OutFile: outPDF,
		Signatures: []PlacedSignature{
			{
				ID:       "sig-0",
				Page:     1,
				X:        30,
				Y:        40,
				Width:    80,
				Height:   40,
				Opacity:  1.0,
				Rotation: 0,
				InkMode:  InkOriginal,
				Image:    sigImg,
			},
			{
				ID:       "sig-90",
				Page:     1,
				X:        40,
				Y:        120,
				Width:    100,
				Height:   60,
				Opacity:  1.0,
				Rotation: 90,
				InkMode:  InkOriginal,
				Image:    sigImg,
			},
		},
	}

	ctx := context.Background()
	if err := SignPDF(ctx, req, nil); err != nil {
		t.Fatalf("SignPDF failed: %v", err)
	}

	renderer := DefaultRenderer()
	configureTestRenderer(t, renderer)
	rendered, err := renderer.RenderPage(ctx, outPDF, 1, 1.0)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}

	b := rendered.Bounds()
	pxPerPtX := float64(b.Dx()) / 200.0
	pxPerPtY := float64(b.Dy()) / 300.0

	measureBlueBounds := func(yStartPt, yEndPt float64) (minX, minY, maxX, maxY float64, count int) {
		minPxX, minPxY := b.Max.X, b.Max.Y
		maxPxX, maxPxY := b.Min.X, b.Min.Y
		y0 := int(yStartPt * pxPerPtY)
		y1 := int(yEndPt * pxPerPtY)
		if y0 < b.Min.Y {
			y0 = b.Min.Y
		}
		if y1 > b.Max.Y {
			y1 = b.Max.Y
		}
		for py := y0; py < y1; py++ {
			for px := b.Min.X; px < b.Max.X; px++ {
				r, g, bl, _ := rendered.At(px, py).RGBA()
				// Detect blue ink (significantly bluer than red/green)
				if bl > 30000 && r < 20000 && g < 20000 {
					count++
					if px < minPxX {
						minPxX = px
					}
					if px > maxPxX {
						maxPxX = px
					}
					if py < minPxY {
						minPxY = py
					}
					if py > maxPxY {
						maxPxY = py
					}
				}
			}
		}
		if count == 0 {
			return 0, 0, 0, 0, 0
		}
		return float64(minPxX-b.Min.X) / pxPerPtX,
			float64(minPxY-b.Min.Y) / pxPerPtY,
			float64(maxPxX-b.Min.X+1) / pxPerPtX,
			float64(maxPxY-b.Min.Y+1) / pxPerPtY,
			count
	}

	assertRect := func(name string, gotX, gotY, gotW, gotH, wantX, wantY, wantW, wantH float64) {
		t.Helper()
		const tol = 3.0
		if abs := func(v float64) float64 {
			if v < 0 {
				return -v
			}
			return v
		}; abs(gotX-wantX) > tol || abs(gotY-wantY) > tol || abs(gotW-wantW) > tol || abs(gotH-wantH) > tol {
			t.Errorf("%s mismatch: got (x=%.1f, y=%.1f, w=%.1f, h=%.1f), want (x=%.1f, y=%.1f, w=%.1f, h=%.1f)",
				name, gotX, gotY, gotW, gotH, wantX, wantY, wantW, wantH)
		}
	}

	x0, y0, x1, y1, c1 := measureBlueBounds(0, 100)
	if c1 == 0 {
		t.Skip("renderer does not rasterize stamped PDF content in this environment")
	}
	assertRect("sig-0", x0, y0, x1-x0, y1-y0, 30, 40, 80, 40)

	rx0, ry0, rx1, ry1, c2 := measureBlueBounds(100, 220)
	if c2 == 0 {
		t.Fatalf("expected rendered pixels for sig-90")
	}
	assertRect("sig-90 (Contain centered)", rx0, ry0, rx1-rx0, ry1-ry0, 75, 120, 30, 60)
}

func TestSignPDFRotatedPageWYSIWYG(t *testing.T) {
	tmpDir := t.TempDir()
	inPDF := filepath.Join(tmpDir, "rotated90.pdf")
	outPDF := filepath.Join(tmpDir, "rotated90_signed.pdf")

	rotatedPDF := strings.Replace(minimalPDF, "/MediaBox [0 0 200 300]", "/MediaBox [0 0 200 300] /Rotate 90", 1)
	if err := os.WriteFile(inPDF, []byte(rotatedPDF), 0644); err != nil {
		t.Fatalf("failed to write rotated test pdf: %v", err)
	}

	ctx := context.Background()
	renderer := DefaultRenderer()
	configureTestRenderer(t, renderer)
	w, h, err := renderer.PageDimensions(ctx, inPDF, 1)
	if err != nil {
		t.Fatalf("PageDimensions failed: %v", err)
	}
	if w != 300 || h != 200 {
		t.Fatalf("expected visual rotated dimensions 300x200, got %.0fx%.0f", w, h)
	}

	sigImg := createSolidTestImage(160, 80, color.RGBA{R: 0, G: 0, B: 220, A: 255})
	req := SignRequest{
		InFile:  inPDF,
		OutFile: outPDF,
		Signatures: []PlacedSignature{
			{
				ID:       "sig-rot-page",
				Page:     1,
				X:        40,
				Y:        30,
				Width:    80,
				Height:   40,
				Opacity:  1.0,
				Rotation: 0,
				InkMode:  InkOriginal,
				Image:    sigImg,
			},
		},
	}

	if err := SignPDF(ctx, req, nil); err != nil {
		t.Fatalf("SignPDF failed: %v", err)
	}

	rendered, err := renderer.RenderPage(ctx, outPDF, 1, 1.0)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}
	b := rendered.Bounds()
	if b.Dx() <= b.Dy() {
		t.Fatalf("expected landscape rendered bitmap for /Rotate 90 page, got %dx%d", b.Dx(), b.Dy())
	}

	pxPerPtX := float64(b.Dx()) / 300.0
	pxPerPtY := float64(b.Dy()) / 200.0
	minPxX, minPxY := b.Max.X, b.Max.Y
	maxPxX, maxPxY := b.Min.X, b.Min.Y
	count := 0
	for py := b.Min.Y; py < b.Max.Y; py++ {
		for px := b.Min.X; px < b.Max.X; px++ {
			r, g, bl, _ := rendered.At(px, py).RGBA()
			if bl > 30000 && r < 20000 && g < 20000 {
				count++
				if px < minPxX {
					minPxX = px
				}
				if px > maxPxX {
					maxPxX = px
				}
				if py < minPxY {
					minPxY = py
				}
				if py > maxPxY {
					maxPxY = py
				}
			}
		}
	}
	if count == 0 {
		t.Skip("renderer does not rasterize stamped PDF content in this environment")
	}

	gotX := float64(minPxX-b.Min.X) / pxPerPtX
	gotY := float64(minPxY-b.Min.Y) / pxPerPtY
	gotW := float64(maxPxX-minPxX+1) / pxPerPtX
	gotH := float64(maxPxY-minPxY+1) / pxPerPtY

	const tol = 3.0
	abs := func(v float64) float64 {
		if v < 0 {
			return -v
		}
		return v
	}
	if abs(gotX-40) > tol || abs(gotY-30) > tol || abs(gotW-80) > tol || abs(gotH-40) > tol {
		t.Errorf("rotated-page sig mismatch: got (x=%.1f, y=%.1f, w=%.1f, h=%.1f), want (40, 30, 80, 40)",
			gotX, gotY, gotW, gotH)
	}
}

func TestSignPDFDeduplicatesXObjects(t *testing.T) {
	tmpDir := t.TempDir()
	single := filepath.Join(tmpDir, "one.pdf")
	if err := os.WriteFile(single, []byte(minimalPDF), 0644); err != nil {
		t.Fatal(err)
	}
	inputs := make([]string, 20)
	for i := range inputs {
		inputs[i] = single
	}
	multiPDF := filepath.Join(tmpDir, "twenty.pdf")
	if err := api.MergeCreateFile(context.Background(), inputs, multiPDF, false, model.NewStatelessConfiguration()); err != nil {
		t.Fatalf("MergeCreateFile: %v", err)
	}

	// Non-uniform 200x100 signature image so PNG stream is non-trivial (~30-60 KB).
	sigImg := image.NewNRGBA(image.Rect(0, 0, 200, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			sigImg.SetNRGBA(x, y, color.NRGBA{
				R: uint8((x*7 + y*13) % 256),
				G: uint8((x*11 + y*5) % 256),
				B: uint8((x*3 + y*17) % 256),
				A: 220,
			})
		}
	}

	placements := make([]PlacedSignature, 20)
	for i := range placements {
		placements[i] = PlacedSignature{
			ID:       "sig",
			Page:     i + 1,
			X:        20,
			Y:        30,
			Width:    80,
			Height:   40,
			Opacity:  1.0,
			Rotation: 0,
			Crop:     DefaultNormalizedRect(),
			InkMode:  InkOriginal,
			Image:    sigImg,
		}
	}

	outPDF := filepath.Join(tmpDir, "signed_twenty.pdf")
	if err := SignPDF(context.Background(), SignRequest{
		InFile:     multiPDF,
		OutFile:    outPDF,
		Signatures: placements,
	}, nil); err != nil {
		t.Fatalf("SignPDF 20 pages: %v", err)
	}

	raw, err := os.ReadFile(outPDF)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("MToolsSig1")) {
		t.Fatal("expected /MToolsSig1 XObject in signed PDF")
	}
	if bytes.Contains(raw, []byte("MToolsSig2")) {
		t.Fatal("expected identical signatures across 20 pages to share a single XObject (/MToolsSig1), found /MToolsSig2")
	}
}
