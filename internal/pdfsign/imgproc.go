package pdfsign

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// InkColorMode defines how ink color is rendered during background removal or drawing.
type InkColorMode int

const (
	InkOriginal InkColorMode = iota
	InkBlack
	InkBlue
	InkRed
)

// NormalizedRect represents a bounding box with coordinates in [0.0, 1.0].
type NormalizedRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

func DefaultNormalizedRect() NormalizedRect {
	return NormalizedRect{X: 0, Y: 0, W: 1, H: 1}
}

// Point represents a 2D coordinate.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Stroke represents a single continuous freehand stroke.
type Stroke struct {
	Points  []Point    `json:"points"`
	Width   float64    `json:"width"`
	Color   color.RGBA `json:"color"`
	Eraser  bool       `json:"eraser"`
}

// PhotoProcessOptions configures paper background removal and recoloring.
type PhotoProcessOptions struct {
	Threshold  float64        // 0.0 to 1.0; luminance above this threshold becomes transparent
	Contrast   float64        // >= 1.0; enhances stroke contrast
	InkMode    InkColorMode   // InkOriginal, InkBlack, InkBlue, InkRed
	Crop       NormalizedRect // Crop window inside source image [0..1]
	EnhanceInk bool           // If true, applies EnhanceSignatureInk after background removal
}

func DefaultPhotoProcessOptions() PhotoProcessOptions {
	return PhotoProcessOptions{
		Threshold: 0.80,
		Contrast:  1.5,
		InkMode:   InkBlack,
		Crop:      DefaultNormalizedRect(),
	}
}

// RemoveBackground strips white/light paper background from an image,
// converting the paper to transparent alpha while keeping or recoloring ink strokes.
func RemoveBackground(src image.Image, opts PhotoProcessOptions) *image.NRGBA {
	bounds := src.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return image.NewNRGBA(image.Rect(0, 0, 1, 1))
	}

	// Calculate crop bounds
	cropX := int(opts.Crop.X * float64(srcW))
	cropY := int(opts.Crop.Y * float64(srcH))
	cropW := int(opts.Crop.W * float64(srcW))
	cropH := int(opts.Crop.H * float64(srcH))

	if cropW <= 0 || cropX >= srcW {
		cropX, cropW = 0, srcW
	}
	if cropH <= 0 || cropY >= srcH {
		cropY, cropH = 0, srcH
	}
	if cropX+cropW > srcW {
		cropW = srcW - cropX
	}
	if cropY+cropH > srcH {
		cropH = srcH - cropY
	}

	dst := image.NewNRGBA(image.Rect(0, 0, cropW, cropH))

	thresh := opts.Threshold
	if thresh <= 0 {
		thresh = 0.80
	}
	contrast := opts.Contrast
	if contrast < 1.0 {
		contrast = 1.0
	}

	for y := 0; y < cropH; y++ {
		for x := 0; x < cropW; x++ {
			c := src.At(bounds.Min.X+cropX+x, bounds.Min.Y+cropY+y)
			r, g, b, a := c.RGBA()
			if a == 0 {
				dst.SetNRGBA(x, y, color.NRGBA{0, 0, 0, 0})
				continue
			}

			// Normalized RGB in [0, 1]
			rf := float64(r) / float64(a)
			gf := float64(g) / float64(a)
			bf := float64(b) / float64(a)

			// Perceptual luminance: 0 (black) to 1 (pure white)
			lum := 0.299*rf + 0.587*gf + 0.114*bf

			// Darkness factor: 1.0 for darkest ink, 0.0 for paper
			// Lum > thresh is paper -> alpha 0
			// Lum <= thresh ramps alpha up
			var alpha float64
			if lum >= thresh {
				alpha = 0
			} else {
				// Linear ramp from thresh down to 0
				rawAlpha := (thresh - lum) / thresh
				// Apply contrast curve
				alpha = math.Pow(rawAlpha, 1.0/contrast)
				if alpha > 1.0 {
					alpha = 1.0
				}
			}

			if alpha <= 0.01 {
				dst.SetNRGBA(x, y, color.NRGBA{0, 0, 0, 0})
				continue
			}

			alphaByte := uint8(math.Round(alpha * 255.0))

			switch opts.InkMode {
			case InkBlack:
				dst.SetNRGBA(x, y, color.NRGBA{R: 31, G: 40, B: 34, A: alphaByte})
			case InkBlue:
				dst.SetNRGBA(x, y, color.NRGBA{R: 29, G: 78, B: 216, A: alphaByte})
			case InkRed:
				dst.SetNRGBA(x, y, color.NRGBA{R: 185, G: 28, B: 28, A: alphaByte})
			case InkOriginal:
				dst.SetNRGBA(x, y, color.NRGBA{
					R: uint8(math.Round(rf * 255.0)),
					G: uint8(math.Round(gf * 255.0)),
					B: uint8(math.Round(bf * 255.0)),
					A: alphaByte,
				})
			}
		}
	}

	if opts.EnhanceInk {
		return EnhanceSignatureInk(dst)
	}
	return dst
}

// AutoTrimAlphaBounds crops away transparent borders from an NRGBA image.
func AutoTrimAlphaBounds(src *image.NRGBA, alphaMin uint8) *image.NRGBA {
	bounds := src.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()
	if w <= 0 || h <= 0 {
		return src
	}

	minX, minY := w, h
	maxX, maxY := -1, -1

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := src.NRGBAAt(x, y)
			if c.A > alphaMin {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}

	if maxX < minX || maxY < minY {
		// Image is completely transparent
		return image.NewNRGBA(image.Rect(0, 0, 1, 1))
	}

	// Add small 2px padding
	pad := 2
	minX = max(0, minX-pad)
	minY = max(0, minY-pad)
	maxX = min(w-1, maxX+pad)
	maxY = min(h-1, maxY+pad)

	trimmedW := maxX - minX + 1
	trimmedH := maxY - minY + 1
	dst := image.NewNRGBA(image.Rect(0, 0, trimmedW, trimmedH))

	for y := 0; y < trimmedH; y++ {
		for x := 0; x < trimmedW; x++ {
			dst.SetNRGBA(x, y, src.NRGBAAt(minX+x, minY+y))
		}
	}

	return dst
}

// RenderStrokesToImage rasterizes freehand strokes onto a transparent NRGBA image.
func RenderStrokesToImage(w, h int, strokes []Stroke) *image.NRGBA {
	if w <= 0 {
		w = 600
	}
	if h <= 0 {
		h = 300
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))

	for _, s := range strokes {
		if len(s.Points) == 0 {
			continue
		}
		radius := s.Width / 2.0
		if radius < 0.5 {
			radius = 1.0
		}

		if len(s.Points) == 1 {
			// Single dot
			drawFilledCircle(dst, s.Points[0].X, s.Points[0].Y, radius, s.Color, s.Eraser)
			continue
		}

		// Draw smooth connected segments
		for i := 0; i < len(s.Points)-1; i++ {
			p0 := s.Points[i]
			p1 := s.Points[i+1]
			drawThickLine(dst, p0.X, p0.Y, p1.X, p1.Y, radius, s.Color, s.Eraser)
		}
	}

	return dst
}

func drawFilledCircle(img *image.NRGBA, cx, cy, r float64, c color.RGBA, eraser bool) {
	bounds := img.Bounds()
	minX := max(bounds.Min.X, int(math.Floor(cx-r)))
	maxX := min(bounds.Max.X-1, int(math.Ceil(cx+r)))
	minY := max(bounds.Min.Y, int(math.Floor(cy-r)))
	maxY := min(bounds.Max.Y-1, int(math.Ceil(cy+r)))

	r2 := r * r
	col := color.NRGBA{R: c.R, G: c.G, B: c.B, A: c.A}
	if eraser {
		col = color.NRGBA{0, 0, 0, 0}
	}

	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			dx := float64(x) - cx
			dy := float64(y) - cy
			if dx*dx+dy*dy <= r2 {
				img.SetNRGBA(x, y, col)
			}
		}
	}
}

func drawThickLine(img *image.NRGBA, x0, y0, x1, y1, r float64, c color.RGBA, eraser bool) {
	dx := x1 - x0
	dy := y1 - y0
	dist := math.Hypot(dx, dy)
	if dist == 0 {
		drawFilledCircle(img, x0, y0, r, c, eraser)
		return
	}

	// Step by 0.5 radius along the line for continuous smooth stroke
	step := math.Max(0.5, r*0.5)
	steps := int(math.Ceil(dist / step))
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		cx := x0 + t*dx
		cy := y0 + t*dy
		drawFilledCircle(img, cx, cy, r, c, eraser)
	}
}

// PlacementRenderOptions configures the final pre-composited signature image for PDF stamping.
type PlacementRenderOptions struct {
	Crop        NormalizedRect // Sub-crop of signature [0..1]
	RotationDeg float64        // Clockwise rotation degrees
	Opacity     float64        // 0.1 to 1.0
}

func DefaultPlacementRenderOptions() PlacementRenderOptions {
	return PlacementRenderOptions{
		Crop:        DefaultNormalizedRect(),
		RotationDeg: 0,
		Opacity:     1.0,
	}
}

// RenderPlacementImage pre-renders the signature with sub-crop, rotation, and opacity
// into a high-DPI transparent NRGBA image.
func RenderPlacementImage(src image.Image, opts PlacementRenderOptions) *image.NRGBA {
	bounds := src.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return image.NewNRGBA(image.Rect(0, 0, 1, 1))
	}

	// 1. Apply sub-crop
	cropX := int(opts.Crop.X * float64(srcW))
	cropY := int(opts.Crop.Y * float64(srcH))
	cropW := int(opts.Crop.W * float64(srcW))
	cropH := int(opts.Crop.H * float64(srcH))

	if cropW <= 0 || cropX >= srcW {
		cropX, cropW = 0, srcW
	}
	if cropH <= 0 || cropY >= srcH {
		cropY, cropH = 0, srcH
	}
	if cropX+cropW > srcW {
		cropW = srcW - cropX
	}
	if cropY+cropH > srcH {
		cropH = srcH - cropY
	}

	cropped := image.NewNRGBA(image.Rect(0, 0, cropW, cropH))
	for y := 0; y < cropH; y++ {
		for x := 0; x < cropW; x++ {
			c := src.At(bounds.Min.X+cropX+x, bounds.Min.Y+cropY+y)
			r, g, b, a := c.RGBA()
			if a == 0 {
				continue
			}
			alpha := float64(a) / 65535.0 * opts.Opacity
			if alpha < 0 {
				alpha = 0
			} else if alpha > 1.0 {
				alpha = 1.0
			}
			cropped.SetNRGBA(x, y, color.NRGBA{
				R: uint8(r >> 8),
				G: uint8(g >> 8),
				B: uint8(b >> 8),
				A: uint8(math.Round(alpha * 255.0)),
			})
		}
	}

	// 2. Rotate if needed
	rot := math.Mod(opts.RotationDeg, 360)
	if rot < 0 {
		rot += 360
	}
	if math.Abs(rot) < 0.1 || math.Abs(rot-360) < 0.1 {
		return cropped
	}

	rad := rot * math.Pi / 180.0
	cosA := math.Cos(rad)
	sinA := math.Sin(rad)

	// Calculate new rotated bounding box
	w0, h0 := float64(cropW), float64(cropH)
	corners := [][2]float64{
		{-w0 / 2, -h0 / 2},
		{w0 / 2, -h0 / 2},
		{w0 / 2, h0 / 2},
		{-w0 / 2, h0 / 2},
	}

	minRotX, maxRotX := math.Inf(1), math.Inf(-1)
	minRotY, maxRotY := math.Inf(1), math.Inf(-1)
	for _, pt := range corners {
		rx := pt[0]*cosA - pt[1]*sinA
		ry := pt[0]*sinA + pt[1]*cosA
		minRotX = math.Min(minRotX, rx)
		maxRotX = math.Max(maxRotX, rx)
		minRotY = math.Min(minRotY, ry)
		maxRotY = math.Max(maxRotY, ry)
	}

	rotW := int(math.Ceil(maxRotX - minRotX))
	rotH := int(math.Ceil(maxRotY - minRotY))
	if rotW <= 0 {
		rotW = 1
	}
	if rotH <= 0 {
		rotH = 1
	}

	dst := image.NewNRGBA(image.Rect(0, 0, rotW, rotH))
	dstCX := float64(rotW) / 2.0
	dstCY := float64(rotH) / 2.0
	srcCX := w0 / 2.0
	srcCY := h0 / 2.0

	// Inverse rotation mapping
	invCos := math.Cos(-rad)
	invSin := math.Sin(-rad)

	for dy := 0; dy < rotH; dy++ {
		for dx := 0; dx < rotW; dx++ {
			xRel := float64(dx) + 0.5 - dstCX
			yRel := float64(dy) + 0.5 - dstCY

			sx := xRel*invCos - yRel*invSin + srcCX - 0.5
			sy := xRel*invSin + yRel*invCos + srcCY - 0.5

			dst.SetNRGBA(dx, dy, sampleNRGBABilinear(cropped, sx, sy))
		}
	}

	return dst
}

// RecolorSignature tints non-transparent pixels of an image according to InkColorMode.
func RecolorSignature(src image.Image, mode InkColorMode) *image.NRGBA {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	if mode == InkOriginal {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.Set(x, y, src.At(bounds.Min.X+x, bounds.Min.Y+y))
			}
		}
		return dst
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := src.At(bounds.Min.X+x, bounds.Min.Y+y)
			r, g, b, a := c.RGBA()
			if a == 0 {
				continue
			}
			alpha8 := uint8(a >> 8)
			var tint color.NRGBA
			switch mode {
			case InkBlack:
				tint = color.NRGBA{R: 20, G: 20, B: 20, A: alpha8}
			case InkBlue:
				tint = color.NRGBA{R: 20, G: 50, B: 180, A: alpha8}
			case InkRed:
				tint = color.NRGBA{R: 190, G: 25, B: 25, A: alpha8}
			default:
				tint = color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: alpha8}
			}
			dst.SetNRGBA(x, y, tint)
		}
	}
	return dst
}

// EncodePNGToBytes helper encodes image.Image to PNG format byte slice.
func EncodePNGToBytes(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DownscaleToMaxDim returns a bilinear-sampled image whose largest side is at most maxDim.
func DownscaleToMaxDim(src image.Image, maxDim int) image.Image {
	if src == nil || maxDim <= 0 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || (w <= maxDim && h <= maxDim) {
		return src
	}
	nrgba := toNRGBA(src)
	scale := float64(maxDim) / float64(max(w, h))
	nw := max(int(math.Round(float64(w)*scale)), 1)
	nh := max(int(math.Round(float64(h)*scale)), 1)
	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		sy := (float64(y)+0.5)*float64(h)/float64(nh) - 0.5
		for x := 0; x < nw; x++ {
			sx := (float64(x)+0.5)*float64(w)/float64(nw) - 0.5
			dst.SetNRGBA(x, y, sampleNRGBABilinear(nrgba, sx, sy))
		}
	}
	return dst
}

// EnhanceSignatureInk sharpens soft ink edges, removes faint semi-transparent halos,
// and smoothly upscales low-resolution cutouts 2x so thin strokes look crisp on canvas and PDF.
func EnhanceSignatureInk(src image.Image) *image.NRGBA {
	if src == nil {
		return image.NewNRGBA(image.Rect(0, 0, 1, 1))
	}
	base := toNRGBA(src)
	bw, bh := base.Bounds().Dx(), base.Bounds().Dy()
	if bw <= 0 || bh <= 0 {
		return base
	}

	work := base
	if max(bw, bh) < 900 {
		nw, nh := bw*2, bh*2
		up := image.NewNRGBA(image.Rect(0, 0, nw, nh))
		for y := 0; y < nh; y++ {
			sy := (float64(y)+0.5)/2.0 - 0.5
			for x := 0; x < nw; x++ {
				sx := (float64(x)+0.5)/2.0 - 0.5
				up.SetNRGBA(x, y, sampleNRGBABilinear(base, sx, sy))
			}
		}
		work = up
	}

	w, h := work.Bounds().Dx(), work.Bounds().Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := work.NRGBAAt(x, y)
			if c.A == 0 {
				continue
			}
			var sumA float64
			var count float64
			for oy := -1; oy <= 1; oy++ {
				ny := y + oy
				if ny < 0 || ny >= h {
					continue
				}
				for ox := -1; ox <= 1; ox++ {
					nx := x + ox
					if nx < 0 || nx >= w {
						continue
					}
					sumA += float64(work.NRGBAAt(nx, ny).A) / 255.0
					count++
				}
			}
			avgA := sumA / count
			a := float64(c.A) / 255.0
			boosted := a + 0.45*(a-avgA)
			t := min(max((boosted-0.24)/(0.58-0.24), 0), 1)
			outA := t * t * (3 - 2*t)
			alphaByte := uint8(math.Round(outA * 255.0))
			if alphaByte <= 2 {
				continue
			}
			dst.SetNRGBA(x, y, color.NRGBA{R: c.R, G: c.G, B: c.B, A: alphaByte})
		}
	}
	return dst
}

func toNRGBA(src image.Image) *image.NRGBA {
	if nrgba, ok := src.(*image.NRGBA); ok {
		b := nrgba.Bounds()
		if b.Min.X == 0 && b.Min.Y == 0 {
			return nrgba
		}
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

func sampleNRGBABilinear(src *image.NRGBA, x, y float64) color.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || x < -0.5 || y < -0.5 || x > float64(w)-0.5 || y > float64(h)-0.5 {
		return color.NRGBA{}
	}
	cx := min(max(x, 0), float64(w-1))
	cy := min(max(y, 0), float64(h-1))
	x0 := int(math.Floor(cx))
	y0 := int(math.Floor(cy))
	x1 := min(x0+1, w-1)
	y1 := min(y0+1, h-1)
	fx := cx - float64(x0)
	fy := cy - float64(y0)

	c00 := src.NRGBAAt(b.Min.X+x0, b.Min.Y+y0)
	c10 := src.NRGBAAt(b.Min.X+x1, b.Min.Y+y0)
	c01 := src.NRGBAAt(b.Min.X+x0, b.Min.Y+y1)
	c11 := src.NRGBAAt(b.Min.X+x1, b.Min.Y+y1)

	w00 := (1 - fx) * (1 - fy)
	w10 := fx * (1 - fy)
	w01 := (1 - fx) * fy
	w11 := fx * fy

	a00, a10, a01, a11 := float64(c00.A), float64(c10.A), float64(c01.A), float64(c11.A)
	a := a00*w00 + a10*w10 + a01*w01 + a11*w11
	if a <= 0.5 {
		return color.NRGBA{}
	}
	r := (float64(c00.R)*a00*w00 + float64(c10.R)*a10*w10 + float64(c01.R)*a01*w01 + float64(c11.R)*a11*w11) / a
	g := (float64(c00.G)*a00*w00 + float64(c10.G)*a10*w10 + float64(c01.G)*a01*w01 + float64(c11.G)*a11*w11) / a
	bl := (float64(c00.B)*a00*w00 + float64(c10.B)*a10*w10 + float64(c01.B)*a01*w01 + float64(c11.B)*a11*w11) / a
	return color.NRGBA{
		R: uint8(min(max(math.Round(r), 0), 255)),
		G: uint8(min(max(math.Round(g), 0), 255)),
		B: uint8(min(max(math.Round(bl), 0), 255)),
		A: uint8(min(max(math.Round(a), 0), 255)),
	}
}

// RemoveBackgroundScaled computes a responsive preview cutout within maxPreviewDim.
func RemoveBackgroundScaled(src image.Image, opts PhotoProcessOptions, maxPreviewDim int) *image.NRGBA {
	return RemoveBackground(DownscaleToMaxDim(src, maxPreviewDim), opts)
}
