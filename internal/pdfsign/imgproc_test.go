package pdfsign_test

import (
	"image"
	"image/color"
	"testing"

	"github.com/seiortech/mtools-plugin-pdfsign/internal/pdfsign"
)

func TestPhotoBackgroundRemoval(t *testing.T) {
	// Create an image with white paper background (RGBA: 250, 250, 250)
	// and a dark ink box in the center (RGBA: 20, 20, 20)
	w, h := 100, 100
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x >= 40 && x < 60 && y >= 40 && y < 60 {
				src.Set(x, y, color.RGBA{R: 20, G: 20, B: 20, A: 255})
			} else {
				src.Set(x, y, color.RGBA{R: 245, G: 245, B: 245, A: 255})
			}
		}
	}

	opts := pdfsign.DefaultPhotoProcessOptions()
	opts.Threshold = 0.85
	opts.InkMode = pdfsign.InkBlack

	dst := pdfsign.RemoveBackground(src, opts)

	// Paper pixels (e.g. at 10, 10) must be transparent
	paperPixel := dst.NRGBAAt(10, 10)
	if paperPixel.A != 0 {
		t.Fatalf("expected paper pixel to be transparent, got alpha=%d", paperPixel.A)
	}

	// Ink pixels (e.g. at 50, 50) must be opaque and recolored to InkBlack
	inkPixel := dst.NRGBAAt(50, 50)
	if inkPixel.A < 200 {
		t.Fatalf("expected ink pixel to be opaque, got alpha=%d", inkPixel.A)
	}
	if inkPixel.R != 31 || inkPixel.G != 40 || inkPixel.B != 34 {
		t.Fatalf("expected InkBlack (31,40,34), got (%d,%d,%d)", inkPixel.R, inkPixel.G, inkPixel.B)
	}
}

func TestAutoTrimAlphaBounds(t *testing.T) {
	// Create a 100x100 transparent image with a 10x10 opaque square from (40,40) to (50,50)
	src := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	for y := 40; y < 50; y++ {
		for x := 40; x < 50; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 20, G: 20, B: 20, A: 255})
		}
	}

	trimmed := pdfsign.AutoTrimAlphaBounds(src, 10)
	b := trimmed.Bounds()
	// Should be trimmed close to ~14x14 with 2px padding
	if b.Dx() < 10 || b.Dx() > 20 || b.Dy() < 10 || b.Dy() > 20 {
		t.Fatalf("unexpected trimmed size: %dx%d", b.Dx(), b.Dy())
	}
}

func TestRenderStrokesAndEraser(t *testing.T) {
	strokes := []pdfsign.Stroke{
		{
			Points: []pdfsign.Point{
				{X: 10, Y: 10},
				{X: 50, Y: 10},
			},
			Width: 4,
			Color: color.RGBA{R: 31, G: 40, B: 34, A: 255},
		},
		{
			Points: []pdfsign.Point{
				{X: 28, Y: 8},
				{X: 32, Y: 12},
			},
			Width: 6,
			Eraser: true, // Erases the middle of the stroke
		},
	}

	img := pdfsign.RenderStrokesToImage(100, 50, strokes)
	if img == nil {
		t.Fatal("expected non-nil rendered strokes image")
	}

	// At (15, 10) should have drawn ink
	c1 := img.NRGBAAt(15, 10)
	if c1.A == 0 {
		t.Fatalf("expected drawn ink at (15, 10), got alpha 0")
	}

	// At (30, 10) should have been erased
	c2 := img.NRGBAAt(30, 10)
	if c2.A != 0 {
		t.Fatalf("expected erased pixel at (30, 10), got alpha %d", c2.A)
	}
}

func TestRenderPlacementRotationAndOpacity(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 50, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 50; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 31, G: 40, B: 34, A: 255})
		}
	}

	opts := pdfsign.PlacementRenderOptions{
		Crop:        pdfsign.DefaultNormalizedRect(),
		RotationDeg: 90,
		Opacity:     0.5,
	}

	dst := pdfsign.RenderPlacementImage(src, opts)
	b := dst.Bounds()

	// 50x20 rotated 90 degrees should be roughly 20x50
	if b.Dx() < 18 || b.Dx() > 22 || b.Dy() < 48 || b.Dy() > 52 {
		t.Fatalf("expected rotated dimensions ~20x50, got %dx%d", b.Dx(), b.Dy())
	}

	// Opacity should be approximately 128 (0.5 * 255)
	midPixel := dst.NRGBAAt(b.Dx()/2, b.Dy()/2)
	if midPixel.A < 110 || midPixel.A > 145 {
		t.Fatalf("expected alpha around 128 for 0.5 opacity, got %d", midPixel.A)
	}
}

func TestEnhanceSignatureInk(t *testing.T) {
	// Small 40x20 image with a blurry semi-transparent halo (A=35) and solid stroke core (A=190).
	src := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for x := 5; x < 35; x++ {
		src.SetNRGBA(x, 8, color.NRGBA{R: 20, G: 20, B: 20, A: 35})
		src.SetNRGBA(x, 9, color.NRGBA{R: 20, G: 20, B: 20, A: 190})
		src.SetNRGBA(x, 10, color.NRGBA{R: 20, G: 20, B: 20, A: 190})
		src.SetNRGBA(x, 11, color.NRGBA{R: 20, G: 20, B: 20, A: 35})
	}

	out := pdfsign.EnhanceSignatureInk(src)
	if out.Bounds().Dx() != 80 || out.Bounds().Dy() != 40 {
		t.Fatalf("expected 2x upscaled dimensions 80x40, got %dx%d", out.Bounds().Dx(), out.Bounds().Dy())
	}
	if halo := out.NRGBAAt(40, 15).A; halo != 0 {
		t.Fatalf("expected outer blurry halo alpha=0, got %d", halo)
	}
	if core := out.NRGBAAt(40, 19).A; core < 230 {
		t.Fatalf("expected core ink alpha solidified >=230, got %d", core)
	}
}

func TestRenderPlacementStraightAlphaNoDarkHalo(t *testing.T) {
	// Straight-alpha semi-transparent pixel: RGB=(200, 100, 50), A=128.
	nrgba := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	nrgba.SetNRGBA(1, 1, color.NRGBA{R: 200, G: 100, B: 50, A: 128})

	outNRGBA := pdfsign.RenderPlacementImage(nrgba, pdfsign.DefaultPlacementRenderOptions())
	p1 := outNRGBA.NRGBAAt(1, 1)
	if p1.R != 200 || p1.G != 100 || p1.B != 50 || p1.A != 128 {
		t.Fatalf("NRGBA straight alpha corrupted: got %+v, want {200 100 50 128}", p1)
	}

	// Premultiplied RGBA representation of RGB=(200, 100, 50), A=128 -> (100, 50, 25, 128).
	rgba := image.NewRGBA(image.Rect(0, 0, 4, 4))
	rgba.SetRGBA(1, 1, color.RGBA{R: 100, G: 50, B: 25, A: 128})
	outRGBA := pdfsign.RenderPlacementImage(rgba, pdfsign.DefaultPlacementRenderOptions())
	p2 := outRGBA.NRGBAAt(1, 1)
	if p2.R < 195 || p2.R > 205 || p2.G < 95 || p2.G > 105 || p2.B < 45 || p2.B > 55 || p2.A != 128 {
		t.Fatalf("RGBA un-premultiplication darkened edge: got %+v, want ~{200 100 50 128}", p2)
	}
}

func TestRenderPlacementExactOrthogonalRotations(t *testing.T) {
	// 4x2 asymmetric image with distinct corner pixels.
	src := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	tl := color.NRGBA{R: 255, G: 0, B: 0, A: 255}
	tr := color.NRGBA{R: 0, G: 255, B: 0, A: 255}
	br := color.NRGBA{R: 0, G: 0, B: 255, A: 255}
	bl := color.NRGBA{R: 255, G: 255, B: 0, A: 255}
	src.SetNRGBA(0, 0, tl)
	src.SetNRGBA(3, 0, tr)
	src.SetNRGBA(3, 1, br)
	src.SetNRGBA(0, 1, bl)

	r90 := pdfsign.RenderPlacementImage(src, pdfsign.PlacementRenderOptions{
		Crop: pdfsign.DefaultNormalizedRect(), RotationDeg: 90, Opacity: 1.0,
	})
	if r90.Bounds().Dx() != 2 || r90.Bounds().Dy() != 4 {
		t.Fatalf("90deg bounds = %v, want 2x4", r90.Bounds())
	}
	if r90.NRGBAAt(1, 0) != tl || r90.NRGBAAt(1, 3) != tr || r90.NRGBAAt(0, 3) != br || r90.NRGBAAt(0, 0) != bl {
		t.Fatalf("90deg corners mismatch: TR=%v BR=%v BL=%v TL=%v",
			r90.NRGBAAt(1, 0), r90.NRGBAAt(1, 3), r90.NRGBAAt(0, 3), r90.NRGBAAt(0, 0))
	}

	r180 := pdfsign.RenderPlacementImage(src, pdfsign.PlacementRenderOptions{
		Crop: pdfsign.DefaultNormalizedRect(), RotationDeg: 180, Opacity: 1.0,
	})
	if r180.NRGBAAt(3, 1) != tl || r180.NRGBAAt(0, 1) != tr || r180.NRGBAAt(0, 0) != br || r180.NRGBAAt(3, 0) != bl {
		t.Fatalf("180deg corners mismatch")
	}

	r270 := pdfsign.RenderPlacementImage(src, pdfsign.PlacementRenderOptions{
		Crop: pdfsign.DefaultNormalizedRect(), RotationDeg: 270, Opacity: 1.0,
	})
	if r270.Bounds().Dx() != 2 || r270.Bounds().Dy() != 4 {
		t.Fatalf("270deg bounds = %v, want 2x4", r270.Bounds())
	}
	if r270.NRGBAAt(0, 3) != tl || r270.NRGBAAt(0, 0) != tr || r270.NRGBAAt(1, 0) != br || r270.NRGBAAt(1, 3) != bl {
		t.Fatalf("270deg corners mismatch")
	}
}

func TestRenderStrokeOntoMatchesFullRender(t *testing.T) {
	strokes := []pdfsign.Stroke{
		{Points: []pdfsign.Point{{X: 10, Y: 10}, {X: 80, Y: 40}}, Width: 4, Color: color.RGBA{R: 20, G: 50, B: 180, A: 255}},
		{Points: []pdfsign.Point{{X: 40, Y: 20}, {X: 50, Y: 30}}, Width: 8, Eraser: true},
	}
	full := pdfsign.RenderStrokesToImage(120, 60, strokes)
	inc := image.NewNRGBA(image.Rect(0, 0, 120, 60))
	for _, s := range strokes {
		pdfsign.RenderStrokeOnto(inc, s)
	}
	if len(full.Pix) != len(inc.Pix) {
		t.Fatalf("pixel length mismatch")
	}
	for i := range full.Pix {
		if full.Pix[i] != inc.Pix[i] {
			t.Fatalf("incremental stroke diverged at byte %d: got %d want %d", i, inc.Pix[i], full.Pix[i])
		}
	}
}

func BenchmarkRemoveBackgroundNRGBA(b *testing.B) {
	src := image.NewNRGBA(image.Rect(0, 0, 1024, 768))
	for y := 0; y < 768; y++ {
		for x := 0; x < 1024; x++ {
			if (x+y)%17 == 0 {
				src.SetNRGBA(x, y, color.NRGBA{R: 25, G: 30, B: 35, A: 255})
			} else {
				src.SetNRGBA(x, y, color.NRGBA{R: 245, G: 244, B: 240, A: 255})
			}
		}
	}
	opts := pdfsign.DefaultPhotoProcessOptions()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pdfsign.RemoveBackground(src, opts)
	}
}

func BenchmarkRemoveBackgroundYCbCr(b *testing.B) {
	src := image.NewYCbCr(image.Rect(0, 0, 1024, 768), image.YCbCrSubsampleRatio420)
	for i := range src.Y {
		if i%17 == 0 {
			src.Y[i] = 30
		} else {
			src.Y[i] = 245
		}
	}
	opts := pdfsign.DefaultPhotoProcessOptions()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pdfsign.RemoveBackground(src, opts)
	}
}

func BenchmarkEnhanceSignatureInk(b *testing.B) {
	src := image.NewNRGBA(image.Rect(0, 0, 400, 200))
	for y := 60; y < 140; y++ {
		for x := 40; x < 360; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 20, G: 20, B: 20, A: 180})
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pdfsign.EnhanceSignatureInk(src)
	}
}

func BenchmarkRenderPlacementRotate90(b *testing.B) {
	src := image.NewNRGBA(image.Rect(0, 0, 1024, 512))
	opts := pdfsign.PlacementRenderOptions{
		Crop:        pdfsign.DefaultNormalizedRect(),
		RotationDeg: 90,
		Opacity:     0.9,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pdfsign.RenderPlacementImage(src, opts)
	}
}
