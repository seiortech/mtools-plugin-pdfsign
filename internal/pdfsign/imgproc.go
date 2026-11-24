package pdfsign

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
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
	Points []Point    `json:"points"`
	Width  float64    `json:"width"`
	Color  color.RGBA `json:"color"`
	Eraser bool       `json:"eraser"`
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

func buildAlphaLUT(thresh, contrast float64) [1025]uint8 {
	var lut [1025]uint8
	invContrast := 1.0 / contrast
	for i := 0; i <= 1024; i++ {
		lum := float64(i) / 1024.0
		if lum >= thresh {
			continue
		}
		rawAlpha := (thresh - lum) / thresh
		alpha := math.Pow(rawAlpha, invContrast)
		if alpha > 1.0 {
			alpha = 1.0
		}
		if alpha <= 0.01 {
			continue
		}
		lut[i] = uint8(math.Round(alpha * 255.0))
	}
	return lut
}

func inkRGBForRemoveBG(mode InkColorMode) (uint8, uint8, uint8, bool) {
	switch mode {
	case InkBlack:
		return 31, 40, 34, true
	case InkBlue:
		return 29, 78, 216, true
	case InkRed:
		return 185, 28, 28, true
	default:
		return 0, 0, 0, false
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

	lut := buildAlphaLUT(thresh, contrast)
	tintR, tintG, tintB, useTint := inkRGBForRemoveBG(opts.InkMode)
	startX := bounds.Min.X + cropX
	startY := bounds.Min.Y + cropY

	switch s := src.(type) {
	case *image.NRGBA:
		for y := 0; y < cropH; y++ {
			srcRow := s.PixOffset(startX, startY+y)
			dstRow := y * dst.Stride
			for x := 0; x < cropW; x++ {
				so := srcRow + x*4
				a := s.Pix[so+3]
				if a == 0 {
					continue
				}
				r, g, b := s.Pix[so], s.Pix[so+1], s.Pix[so+2]
				lumIdx := (306*int(r) + 601*int(g) + 117*int(b) + 127) / 255
				alphaByte := lut[lumIdx]
				if alphaByte == 0 {
					continue
				}
				do := dstRow + x*4
				if useTint {
					dst.Pix[do] = tintR
					dst.Pix[do+1] = tintG
					dst.Pix[do+2] = tintB
				} else {
					dst.Pix[do] = r
					dst.Pix[do+1] = g
					dst.Pix[do+2] = b
				}
				dst.Pix[do+3] = alphaByte
			}
		}

	case *image.RGBA:
		for y := 0; y < cropH; y++ {
			srcRow := s.PixOffset(startX, startY+y)
			dstRow := y * dst.Stride
			for x := 0; x < cropW; x++ {
				so := srcRow + x*4
				a := s.Pix[so+3]
				if a == 0 {
					continue
				}
				r, g, b := s.Pix[so], s.Pix[so+1], s.Pix[so+2]
				if a != 255 {
					ai := int(a)
					r = uint8(min((int(r)*255+ai/2)/ai, 255))
					g = uint8(min((int(g)*255+ai/2)/ai, 255))
					b = uint8(min((int(b)*255+ai/2)/ai, 255))
				}
				lumIdx := (306*int(r) + 601*int(g) + 117*int(b) + 127) / 255
				alphaByte := lut[lumIdx]
				if alphaByte == 0 {
					continue
				}
				do := dstRow + x*4
				if useTint {
					dst.Pix[do] = tintR
					dst.Pix[do+1] = tintG
					dst.Pix[do+2] = tintB
				} else {
					dst.Pix[do] = r
					dst.Pix[do+1] = g
					dst.Pix[do+2] = b
				}
				dst.Pix[do+3] = alphaByte
			}
		}

	case *image.YCbCr:
		for y := 0; y < cropH; y++ {
			sy := startY + y
			dstRow := y * dst.Stride
			for x := 0; x < cropW; x++ {
				sx := startX + x
				yv := s.Y[s.YOffset(sx, sy)]
				if useTint {
					lumIdx := (int(yv)*1024 + 127) / 255
					alphaByte := lut[lumIdx]
					if alphaByte == 0 {
						continue
					}
					do := dstRow + x*4
					dst.Pix[do] = tintR
					dst.Pix[do+1] = tintG
					dst.Pix[do+2] = tintB
					dst.Pix[do+3] = alphaByte
					continue
				}
				co := s.COffset(sx, sy)
				r, g, b := color.YCbCrToRGB(yv, s.Cb[co], s.Cr[co])
				lumIdx := (306*int(r) + 601*int(g) + 117*int(b) + 127) / 255
				alphaByte := lut[lumIdx]
				if alphaByte == 0 {
					continue
				}
				do := dstRow + x*4
				dst.Pix[do] = r
				dst.Pix[do+1] = g
				dst.Pix[do+2] = b
				dst.Pix[do+3] = alphaByte
			}
		}

	default:
		for y := 0; y < cropH; y++ {
			dstRow := y * dst.Stride
			for x := 0; x < cropW; x++ {
				r, g, b, a := src.At(startX+x, startY+y).RGBA()
				if a == 0 {
					continue
				}
				r8 := uint8(min((r*255+a/2)/a, 255))
				g8 := uint8(min((g*255+a/2)/a, 255))
				b8 := uint8(min((b*255+a/2)/a, 255))
				lumIdx := (306*int(r8) + 601*int(g8) + 117*int(b8) + 127) / 255
				alphaByte := lut[lumIdx]
				if alphaByte == 0 {
					continue
				}
				do := dstRow + x*4
				if useTint {
					dst.Pix[do] = tintR
					dst.Pix[do+1] = tintG
					dst.Pix[do+2] = tintB
				} else {
					dst.Pix[do] = r8
					dst.Pix[do+1] = g8
					dst.Pix[do+2] = b8
				}
				dst.Pix[do+3] = alphaByte
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
		rowOff := src.PixOffset(bounds.Min.X, bounds.Min.Y+y)
		for x := 0; x < w; x++ {
			if src.Pix[rowOff+x*4+3] > alphaMin {
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
		return image.NewNRGBA(image.Rect(0, 0, 1, 1))
	}

	const pad = 2
	minX = max(0, minX-pad)
	minY = max(0, minY-pad)
	maxX = min(w-1, maxX+pad)
	maxY = min(h-1, maxY+pad)

	trimmedW := maxX - minX + 1
	trimmedH := maxY - minY + 1
	dst := image.NewNRGBA(image.Rect(0, 0, trimmedW, trimmedH))
	rowBytes := trimmedW * 4

	for y := 0; y < trimmedH; y++ {
		srcOff := src.PixOffset(bounds.Min.X+minX, bounds.Min.Y+minY+y)
		dstOff := y * dst.Stride
		copy(dst.Pix[dstOff:dstOff+rowBytes], src.Pix[srcOff:srcOff+rowBytes])
	}

	return dst
}

// RenderStrokeOnto rasterizes a single freehand stroke incrementally onto an existing NRGBA canvas.
func RenderStrokeOnto(dst *image.NRGBA, s Stroke) {
	if dst == nil || len(s.Points) == 0 {
		return
	}
	radius := s.Width / 2.0
	if radius < 0.5 {
		radius = 1.0
	}
	if len(s.Points) == 1 {
		drawFilledCircle(dst, s.Points[0].X, s.Points[0].Y, radius, s.Color, s.Eraser)
		return
	}
	for i := 0; i < len(s.Points)-1; i++ {
		p0 := s.Points[i]
		p1 := s.Points[i+1]
		drawThickLine(dst, p0.X, p0.Y, p1.X, p1.Y, radius, s.Color, s.Eraser)
	}
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
		RenderStrokeOnto(dst, s)
	}
	return dst
}

func drawFilledCircle(img *image.NRGBA, cx, cy, r float64, c color.RGBA, eraser bool) {
	bounds := img.Bounds()
	minX := max(bounds.Min.X, int(math.Floor(cx-r)))
	maxX := min(bounds.Max.X-1, int(math.Ceil(cx+r)))
	minY := max(bounds.Min.Y, int(math.Floor(cy-r)))
	maxY := min(bounds.Max.Y-1, int(math.Ceil(cy+r)))
	if minX > maxX || minY > maxY {
		return
	}

	r2 := r * r
	cr, cg, cb, ca := c.R, c.G, c.B, c.A
	if eraser {
		cr, cg, cb, ca = 0, 0, 0, 0
	}

	for y := minY; y <= maxY; y++ {
		dy := float64(y) - cy
		dy2 := dy * dy
		rowOff := img.PixOffset(0, y)
		for x := minX; x <= maxX; x++ {
			dx := float64(x) - cx
			if dx*dx+dy2 <= r2 {
				off := rowOff + x*4
				img.Pix[off] = cr
				img.Pix[off+1] = cg
				img.Pix[off+2] = cb
				img.Pix[off+3] = ca
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
// into a straight-alpha transparent NRGBA image.
func RenderPlacementImage(src image.Image, opts PlacementRenderOptions) *image.NRGBA {
	bounds := src.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return image.NewNRGBA(image.Rect(0, 0, 1, 1))
	}

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

	opacity := opts.Opacity
	if opacity < 0 {
		opacity = 0
	} else if opacity > 1.0 {
		opacity = 1.0
	}

	cropped := image.NewNRGBA(image.Rect(0, 0, cropW, cropH))
	startX := bounds.Min.X + cropX
	startY := bounds.Min.Y + cropY

	switch s := src.(type) {
	case *image.NRGBA:
		if opacity >= 0.9999 {
			rowBytes := cropW * 4
			for y := 0; y < cropH; y++ {
				srcOff := s.PixOffset(startX, startY+y)
				dstOff := y * cropped.Stride
				copy(cropped.Pix[dstOff:dstOff+rowBytes], s.Pix[srcOff:srcOff+rowBytes])
			}
		} else {
			for y := 0; y < cropH; y++ {
				srcRow := s.PixOffset(startX, startY+y)
				dstRow := y * cropped.Stride
				for x := 0; x < cropW; x++ {
					so := srcRow + x*4
					a := s.Pix[so+3]
					if a == 0 {
						continue
					}
					do := dstRow + x*4
					cropped.Pix[do] = s.Pix[so]
					cropped.Pix[do+1] = s.Pix[so+1]
					cropped.Pix[do+2] = s.Pix[so+2]
					cropped.Pix[do+3] = uint8(math.Round(float64(a) * opacity))
				}
			}
		}
	default:
		// Un-premultiply RGBA() channels so semi-transparent anti-aliased ink edges never darken.
		for y := 0; y < cropH; y++ {
			dstRow := y * cropped.Stride
			for x := 0; x < cropW; x++ {
				r, g, b, a := src.At(startX+x, startY+y).RGBA()
				if a == 0 {
					continue
				}
				do := dstRow + x*4
				cropped.Pix[do] = uint8(min((r*255+a/2)/a, 255))
				cropped.Pix[do+1] = uint8(min((g*255+a/2)/a, 255))
				cropped.Pix[do+2] = uint8(min((b*255+a/2)/a, 255))
				cropped.Pix[do+3] = uint8(math.Round((float64(a) / 65535.0) * opacity * 255.0))
			}
		}
	}

	rot := math.Mod(opts.RotationDeg, 360)
	if rot < 0 {
		rot += 360
	}
	if math.Abs(rot) < 0.1 || math.Abs(rot-360) < 0.1 {
		return cropped
	}

	// Exact lossless fast paths for orthogonal 90/180/270 degree clockwise rotations.
	if math.Abs(rot-90) < 0.1 {
		dst := image.NewNRGBA(image.Rect(0, 0, cropH, cropW))
		for sy := 0; sy < cropH; sy++ {
			srcRow := sy * cropped.Stride
			dx := cropH - 1 - sy
			for sx := 0; sx < cropW; sx++ {
				so := srcRow + sx*4
				do := sx*dst.Stride + dx*4
				copy(dst.Pix[do:do+4], cropped.Pix[so:so+4])
			}
		}
		return dst
	}
	if math.Abs(rot-180) < 0.1 {
		dst := image.NewNRGBA(image.Rect(0, 0, cropW, cropH))
		for sy := 0; sy < cropH; sy++ {
			srcRow := sy * cropped.Stride
			dstRow := (cropH - 1 - sy) * dst.Stride
			for sx := 0; sx < cropW; sx++ {
				so := srcRow + sx*4
				do := dstRow + (cropW-1-sx)*4
				copy(dst.Pix[do:do+4], cropped.Pix[so:so+4])
			}
		}
		return dst
	}
	if math.Abs(rot-270) < 0.1 {
		dst := image.NewNRGBA(image.Rect(0, 0, cropH, cropW))
		for sy := 0; sy < cropH; sy++ {
			srcRow := sy * cropped.Stride
			dx := sy
			for sx := 0; sx < cropW; sx++ {
				so := srcRow + sx*4
				do := (cropW-1-sx)*dst.Stride + dx*4
				copy(dst.Pix[do:do+4], cropped.Pix[so:so+4])
			}
		}
		return dst
	}

	rad := rot * math.Pi / 180.0
	cosA := math.Cos(rad)
	sinA := math.Sin(rad)

	w0, h0 := float64(cropW), float64(cropH)
	corners := [4][2]float64{
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

	rotW := max(int(math.Ceil(maxRotX-minRotX)), 1)
	rotH := max(int(math.Ceil(maxRotY-minRotY)), 1)

	dst := image.NewNRGBA(image.Rect(0, 0, rotW, rotH))
	dstCX := float64(rotW) / 2.0
	dstCY := float64(rotH) / 2.0
	srcCX := w0 / 2.0
	srcCY := h0 / 2.0

	invCos := math.Cos(-rad)
	invSin := math.Sin(-rad)

	for dy := 0; dy < rotH; dy++ {
		yRel := float64(dy) + 0.5 - dstCY
		dstRow := dy * dst.Stride
		for dx := 0; dx < rotW; dx++ {
			xRel := float64(dx) + 0.5 - dstCX
			sx := xRel*invCos - yRel*invSin + srcCX - 0.5
			sy := xRel*invSin + yRel*invCos + srcCY - 0.5
			c := sampleNRGBABilinear(cropped, sx, sy)
			do := dstRow + dx*4
			dst.Pix[do] = c.R
			dst.Pix[do+1] = c.G
			dst.Pix[do+2] = c.B
			dst.Pix[do+3] = c.A
		}
	}

	return dst
}

// RecolorSignature tints non-transparent pixels of an image according to InkColorMode.
func RecolorSignature(src image.Image, mode InkColorMode) *image.NRGBA {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	if w <= 0 || h <= 0 {
		return dst
	}
	if mode == InkOriginal {
		draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
		return dst
	}

	var tintR, tintG, tintB uint8
	knownTint := true
	switch mode {
	case InkBlack:
		tintR, tintG, tintB = 20, 20, 20
	case InkBlue:
		tintR, tintG, tintB = 20, 50, 180
	case InkRed:
		tintR, tintG, tintB = 190, 25, 25
	default:
		knownTint = false
	}

	if knownTint {
		switch s := src.(type) {
		case *image.NRGBA:
			for y := 0; y < h; y++ {
				srcRow := s.PixOffset(bounds.Min.X, bounds.Min.Y+y)
				dstRow := y * dst.Stride
				for x := 0; x < w; x++ {
					a := s.Pix[srcRow+x*4+3]
					if a == 0 {
						continue
					}
					do := dstRow + x*4
					dst.Pix[do] = tintR
					dst.Pix[do+1] = tintG
					dst.Pix[do+2] = tintB
					dst.Pix[do+3] = a
				}
			}
			return dst
		case *image.RGBA:
			for y := 0; y < h; y++ {
				srcRow := s.PixOffset(bounds.Min.X, bounds.Min.Y+y)
				dstRow := y * dst.Stride
				for x := 0; x < w; x++ {
					a := s.Pix[srcRow+x*4+3]
					if a == 0 {
						continue
					}
					do := dstRow + x*4
					dst.Pix[do] = tintR
					dst.Pix[do+1] = tintG
					dst.Pix[do+2] = tintB
					dst.Pix[do+3] = a
				}
			}
			return dst
		}
	}

	for y := 0; y < h; y++ {
		dstRow := y * dst.Stride
		for x := 0; x < w; x++ {
			r, g, b, a := src.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			if a == 0 {
				continue
			}
			do := dstRow + x*4
			if knownTint {
				dst.Pix[do] = tintR
				dst.Pix[do+1] = tintG
				dst.Pix[do+2] = tintB
			} else {
				dst.Pix[do] = uint8(min((r*255+a/2)/a, 255))
				dst.Pix[do+1] = uint8(min((g*255+a/2)/a, 255))
				dst.Pix[do+2] = uint8(min((b*255+a/2)/a, 255))
			}
			dst.Pix[do+3] = uint8(a >> 8)
		}
	}
	return dst
}

// EncodePNGToBytes encodes image.Image to PNG using default compression (for persistent vault/PDF export).
func EncodePNGToBytes(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// EncodeFastPNGToBytes encodes image.Image using png.BestSpeed for ephemeral live UI preview assets.
func EncodeFastPNGToBytes(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
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
		dstRow := y * dst.Stride
		for x := 0; x < nw; x++ {
			sx := (float64(x)+0.5)*float64(w)/float64(nw) - 0.5
			c := sampleNRGBABilinear(nrgba, sx, sy)
			do := dstRow + x*4
			dst.Pix[do] = c.R
			dst.Pix[do+1] = c.G
			dst.Pix[do+2] = c.B
			dst.Pix[do+3] = c.A
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
			dstRow := y * up.Stride
			for x := 0; x < nw; x++ {
				sx := (float64(x)+0.5)/2.0 - 0.5
				c := sampleNRGBABilinear(base, sx, sy)
				do := dstRow + x*4
				up.Pix[do] = c.R
				up.Pix[do+1] = c.G
				up.Pix[do+2] = c.B
				up.Pix[do+3] = c.A
			}
		}
		work = up
	}

	w, h := work.Bounds().Dx(), work.Bounds().Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	wPix := work.Pix
	wStride := work.Stride

	for y := 0; y < h; y++ {
		rowOff := y * wStride
		dstRow := y * dst.Stride
		yMin := max(y-1, 0)
		yMax := min(y+1, h-1)
		for x := 0; x < w; x++ {
			so := rowOff + x*4
			aByte := wPix[so+3]
			if aByte == 0 {
				continue
			}
			xMin := max(x-1, 0)
			xMax := min(x+1, w-1)
			var sumA int
			count := (yMax - yMin + 1) * (xMax - xMin + 1)
			for ny := yMin; ny <= yMax; ny++ {
				nRow := ny * wStride
				for nx := xMin; nx <= xMax; nx++ {
					sumA += int(wPix[nRow+nx*4+3])
				}
			}
			avgA := float64(sumA) / (float64(count) * 255.0)
			a := float64(aByte) / 255.0
			boosted := a + 0.45*(a-avgA)
			t := min(max((boosted-0.24)/(0.58-0.24), 0), 1)
			outA := t * t * (3 - 2*t)
			alphaByte := uint8(math.Round(outA * 255.0))
			if alphaByte <= 2 {
				continue
			}
			do := dstRow + x*4
			dst.Pix[do] = wPix[so]
			dst.Pix[do+1] = wPix[so+1]
			dst.Pix[do+2] = wPix[so+2]
			dst.Pix[do+3] = alphaByte
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
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
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

	pix := src.Pix
	o00 := src.PixOffset(b.Min.X+x0, b.Min.Y+y0)
	o10 := src.PixOffset(b.Min.X+x1, b.Min.Y+y0)
	o01 := src.PixOffset(b.Min.X+x0, b.Min.Y+y1)
	o11 := src.PixOffset(b.Min.X+x1, b.Min.Y+y1)

	w00 := (1 - fx) * (1 - fy)
	w10 := fx * (1 - fy)
	w01 := (1 - fx) * fy
	w11 := fx * fy

	a00, a10, a01, a11 := float64(pix[o00+3]), float64(pix[o10+3]), float64(pix[o01+3]), float64(pix[o11+3])
	a := a00*w00 + a10*w10 + a01*w01 + a11*w11
	if a <= 0.5 {
		return color.NRGBA{}
	}
	wa00, wa10, wa01, wa11 := a00*w00, a10*w10, a01*w01, a11*w11
	r := (float64(pix[o00])*wa00 + float64(pix[o10])*wa10 + float64(pix[o01])*wa01 + float64(pix[o11])*wa11) / a
	g := (float64(pix[o00+1])*wa00 + float64(pix[o10+1])*wa10 + float64(pix[o01+1])*wa01 + float64(pix[o11+1])*wa11) / a
	bl := (float64(pix[o00+2])*wa00 + float64(pix[o10+2])*wa10 + float64(pix[o01+2])*wa01 + float64(pix[o11+2])*wa11) / a
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
