//go:build darwin

package pdfsign

import (
	"context"
	"fmt"
	"image"
	"math"
	"os"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

type cgRect struct {
	origin cgPoint
	size   cgSize
}

type cgPoint struct {
	x, y float64
}

type cgSize struct {
	width, height float64
}

var (
	darwinInitOnce sync.Once
	darwinInitErr  error

	cgDataProviderCreateWithData func(info unsafe.Pointer, data unsafe.Pointer, size uintptr, releaseData unsafe.Pointer) uintptr
	cgPDFDocumentCreateWithProvider func(provider uintptr) uintptr
	cgPDFDocumentGetNumberOfPages func(doc uintptr) uintptr
	cgPDFDocumentGetPage func(doc uintptr, pageNum uintptr) uintptr
	cgPDFPageGetBoxRect func(page uintptr, box int32) cgRect
	cgPDFPageGetRotationAngle func(page uintptr) int32
	cgColorSpaceCreateDeviceRGB func() uintptr
	cgBitmapContextCreate func(data unsafe.Pointer, width, height, bitsPerComp, bytesPerRow, space uintptr, bitmapInfo uint32) uintptr
	cgBitmapContextGetData func(ctx uintptr) unsafe.Pointer
	cgContextSetRGBFillColor func(ctx uintptr, r, g, b, a float64)
	cgContextFillRect func(ctx uintptr, rect cgRect)
	cgContextSaveGState func(ctx uintptr)
	cgContextTranslateCTM func(ctx uintptr, tx, ty float64)
	cgContextScaleCTM func(ctx uintptr, sx, sy float64)
	cgContextRotateCTM func(ctx uintptr, angle float64)
	cgContextDrawPDFPage func(ctx uintptr, page uintptr)
	cgContextRestoreGState func(ctx uintptr)
	cgContextSetAllowsAntialiasing func(ctx uintptr, allows bool)
	cgContextSetShouldAntialias func(ctx uintptr, should bool)
	cgContextSetShouldSmoothFonts func(ctx uintptr, should bool)
	cgContextSetInterpolationQuality func(ctx uintptr, quality int32)
	cgContextRelease func(ctx uintptr)
	cgColorSpaceRelease func(space uintptr)
	cgPDFDocumentRelease func(doc uintptr)
	cgDataProviderRelease func(provider uintptr)
)

const (
	kCGPDFMediaBox = 0
	kCGPDFCropBox  = 1

	kCGImageAlphaPremultipliedLast = 1
	kCGBitmapByteOrder32Big        = 4 << 12

	kCGInterpolationHigh = 3
)

func initCoreGraphics() error {
	darwinInitOnce.Do(func() {
		cgLib, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			darwinInitErr = fmt.Errorf("failed to dlopen CoreGraphics: %w", err)
			return
		}

		purego.RegisterLibFunc(&cgDataProviderCreateWithData, cgLib, "CGDataProviderCreateWithData")
		purego.RegisterLibFunc(&cgPDFDocumentCreateWithProvider, cgLib, "CGPDFDocumentCreateWithProvider")
		purego.RegisterLibFunc(&cgPDFDocumentGetNumberOfPages, cgLib, "CGPDFDocumentGetNumberOfPages")
		purego.RegisterLibFunc(&cgPDFDocumentGetPage, cgLib, "CGPDFDocumentGetPage")
		purego.RegisterLibFunc(&cgPDFPageGetBoxRect, cgLib, "CGPDFPageGetBoxRect")
		purego.RegisterLibFunc(&cgPDFPageGetRotationAngle, cgLib, "CGPDFPageGetRotationAngle")
		purego.RegisterLibFunc(&cgColorSpaceCreateDeviceRGB, cgLib, "CGColorSpaceCreateDeviceRGB")
		purego.RegisterLibFunc(&cgBitmapContextCreate, cgLib, "CGBitmapContextCreate")
		purego.RegisterLibFunc(&cgBitmapContextGetData, cgLib, "CGBitmapContextGetData")
		purego.RegisterLibFunc(&cgContextSetRGBFillColor, cgLib, "CGContextSetRGBFillColor")
		purego.RegisterLibFunc(&cgContextFillRect, cgLib, "CGContextFillRect")
		purego.RegisterLibFunc(&cgContextSaveGState, cgLib, "CGContextSaveGState")
		purego.RegisterLibFunc(&cgContextTranslateCTM, cgLib, "CGContextTranslateCTM")
		purego.RegisterLibFunc(&cgContextScaleCTM, cgLib, "CGContextScaleCTM")
		purego.RegisterLibFunc(&cgContextRotateCTM, cgLib, "CGContextRotateCTM")
		purego.RegisterLibFunc(&cgContextDrawPDFPage, cgLib, "CGContextDrawPDFPage")
		purego.RegisterLibFunc(&cgContextRestoreGState, cgLib, "CGContextRestoreGState")
		purego.RegisterLibFunc(&cgContextSetAllowsAntialiasing, cgLib, "CGContextSetAllowsAntialiasing")
		purego.RegisterLibFunc(&cgContextSetShouldAntialias, cgLib, "CGContextSetShouldAntialias")
		purego.RegisterLibFunc(&cgContextSetShouldSmoothFonts, cgLib, "CGContextSetShouldSmoothFonts")
		purego.RegisterLibFunc(&cgContextSetInterpolationQuality, cgLib, "CGContextSetInterpolationQuality")
		purego.RegisterLibFunc(&cgContextRelease, cgLib, "CGContextRelease")
		purego.RegisterLibFunc(&cgColorSpaceRelease, cgLib, "CGColorSpaceRelease")
		purego.RegisterLibFunc(&cgPDFDocumentRelease, cgLib, "CGPDFDocumentRelease")
		purego.RegisterLibFunc(&cgDataProviderRelease, cgLib, "CGDataProviderRelease")
	})
	return darwinInitErr
}

// DarwinCoreGraphicsRenderer renders PDF pages natively using macOS CoreGraphics via purego.
type DarwinCoreGraphicsRenderer struct {
	fallback PageRenderer

	fileMu     sync.Mutex
	cachedPath string
	cachedSize int64
	cachedMod  int64
	cachedData []byte
}

func (r *DarwinCoreGraphicsRenderer) readPDFBytes(pdfPath string) ([]byte, error) {
	info, err := os.Stat(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("stat pdf %s: %w", pdfPath, err)
	}
	size := info.Size()
	mod := info.ModTime().UnixNano()

	r.fileMu.Lock()
	if r.cachedPath == pdfPath && r.cachedSize == size && r.cachedMod == mod && len(r.cachedData) > 0 {
		data := r.cachedData
		r.fileMu.Unlock()
		return data, nil
	}
	r.fileMu.Unlock()

	data, err := os.ReadFile(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("read pdf %s: %w", pdfPath, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty pdf %s", pdfPath)
	}

	r.fileMu.Lock()
	r.cachedPath = pdfPath
	r.cachedSize = size
	r.cachedMod = mod
	r.cachedData = data
	r.fileMu.Unlock()
	return data, nil
}

func defaultPlatformRenderer() PageRenderer {
	return &DarwinCoreGraphicsRenderer{
		fallback: &FallbackRenderer{},
	}
}

func (r *DarwinCoreGraphicsRenderer) PageCount(ctx context.Context, pdfPath string) (int, error) {
	if err := initCoreGraphics(); err != nil {
		return r.fallback.PageCount(ctx, pdfPath)
	}

	data, err := os.ReadFile(pdfPath)
	if err != nil {
		return 0, fmt.Errorf("read pdf %s: %w", pdfPath, err)
	}

	provider := cgDataProviderCreateWithData(nil, unsafe.Pointer(&data[0]), uintptr(len(data)), nil)
	if provider == 0 {
		return r.fallback.PageCount(ctx, pdfPath)
	}
	defer cgDataProviderRelease(provider)

	doc := cgPDFDocumentCreateWithProvider(provider)
	if doc == 0 {
		return r.fallback.PageCount(ctx, pdfPath)
	}
	defer cgPDFDocumentRelease(doc)

	return int(cgPDFDocumentGetNumberOfPages(doc)), nil
}

func (r *DarwinCoreGraphicsRenderer) PageDimensions(ctx context.Context, pdfPath string, pageNum int) (float64, float64, error) {
	// pdfcpu provides fast, accurate dimensions directly in pure Go without memory leaks or disk writes.
	f, err := os.Open(pdfPath)
	if err == nil {
		dims, err := api.PageDims(ctx, f, model.NewStatelessConfiguration())
		f.Close()
		if err == nil && pageNum >= 1 && pageNum <= len(dims) {
			dim := dims[pageNum-1]
			return dim.Width, dim.Height, nil
		}
	}

	if err := initCoreGraphics(); err != nil {
		return r.fallback.PageDimensions(ctx, pdfPath, pageNum)
	}

	data, err := os.ReadFile(pdfPath)
	if err != nil {
		return 0, 0, fmt.Errorf("read pdf %s: %w", pdfPath, err)
	}

	provider := cgDataProviderCreateWithData(nil, unsafe.Pointer(&data[0]), uintptr(len(data)), nil)
	if provider == 0 {
		return 0, 0, fmt.Errorf("failed to create data provider")
	}
	defer cgDataProviderRelease(provider)

	doc := cgPDFDocumentCreateWithProvider(provider)
	if doc == 0 {
		return 0, 0, fmt.Errorf("failed to create pdf document")
	}
	defer cgPDFDocumentRelease(doc)

	total := int(cgPDFDocumentGetNumberOfPages(doc))
	if pageNum < 1 || pageNum > total {
		return 0, 0, ErrPageOutOfRange
	}

	page := cgPDFDocumentGetPage(doc, uintptr(pageNum))
	if page == 0 {
		return 0, 0, fmt.Errorf("failed to get page %d", pageNum)
	}

	box := cgPDFPageGetBoxRect(page, kCGPDFCropBox)
	if box.size.width <= 0 || box.size.height <= 0 {
		box = cgPDFPageGetBoxRect(page, kCGPDFMediaBox)
	}
	return box.size.width, box.size.height, nil
}

func (r *DarwinCoreGraphicsRenderer) RenderPage(ctx context.Context, pdfPath string, pageNum int, scale float64) (image.Image, error) {
	if scale <= 0 {
		scale = 1.0
	}

	if err := initCoreGraphics(); err != nil {
		return r.fallback.RenderPage(ctx, pdfPath, pageNum, scale)
	}

	data, err := r.readPDFBytes(pdfPath)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty pdf %s", pdfPath)
	}

	provider := cgDataProviderCreateWithData(nil, unsafe.Pointer(&data[0]), uintptr(len(data)), nil)
	if provider == 0 {
		return r.fallback.RenderPage(ctx, pdfPath, pageNum, scale)
	}
	defer cgDataProviderRelease(provider)

	doc := cgPDFDocumentCreateWithProvider(provider)
	if doc == 0 {
		return r.fallback.RenderPage(ctx, pdfPath, pageNum, scale)
	}
	defer cgPDFDocumentRelease(doc)

	total := int(cgPDFDocumentGetNumberOfPages(doc))
	if pageNum < 1 || pageNum > total {
		return nil, ErrPageOutOfRange
	}

	page := cgPDFDocumentGetPage(doc, uintptr(pageNum))
	if page == 0 {
		return nil, fmt.Errorf("failed to get page %d", pageNum)
	}

	box := cgPDFPageGetBoxRect(page, kCGPDFCropBox)
	if box.size.width <= 0 || box.size.height <= 0 {
		box = cgPDFPageGetBoxRect(page, kCGPDFMediaBox)
	}

	rot := 0
	if cgPDFPageGetRotationAngle != nil {
		rot = ((int(cgPDFPageGetRotationAngle(page)) % 360) + 360) % 360
	}
	visW := box.size.width
	visH := box.size.height
	if rot == 90 || rot == 270 {
		visW, visH = visH, visW
	}

	// Render at 2.0x HiDPI supersampling for retina displays and crisp text/vector edges.
	const renderHiDPIScale = 2.0
	actualScale := scale * renderHiDPIScale

	w := int(math.Ceil(visW * actualScale))
	h := int(math.Ceil(visH * actualScale))
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid page dimensions: %vx%v", w, h)
	}

	colorSpace := cgColorSpaceCreateDeviceRGB()
	if colorSpace == 0 {
		return nil, fmt.Errorf("failed to create device rgb color space")
	}
	defer cgColorSpaceRelease(colorSpace)

	bitmapInfo := uint32(kCGBitmapByteOrder32Big | kCGImageAlphaPremultipliedLast)
	cgCtx := cgBitmapContextCreate(nil, uintptr(w), uintptr(h), 8, uintptr(w*4), colorSpace, bitmapInfo)
	if cgCtx == 0 {
		return nil, fmt.Errorf("failed to create bitmap context")
	}
	defer cgContextRelease(cgCtx)

	// Configure crisp text and vector smoothing
	if cgContextSetAllowsAntialiasing != nil {
		cgContextSetAllowsAntialiasing(cgCtx, true)
	}
	if cgContextSetShouldAntialias != nil {
		cgContextSetShouldAntialias(cgCtx, true)
	}
	if cgContextSetShouldSmoothFonts != nil {
		cgContextSetShouldSmoothFonts(cgCtx, true)
	}
	if cgContextSetInterpolationQuality != nil {
		cgContextSetInterpolationQuality(cgCtx, kCGInterpolationHigh)
	}

	// Fill background with white
	cgContextSetRGBFillColor(cgCtx, 1.0, 1.0, 1.0, 1.0)
	cgContextFillRect(cgCtx, cgRect{origin: cgPoint{0, 0}, size: cgSize{float64(w), float64(h)}})

	// Draw PDF page scaled and rotated to match visual page orientation.
	cgContextSaveGState(cgCtx)
	cgContextScaleCTM(cgCtx, actualScale, actualScale)
	switch rot {
	case 90:
		cgContextTranslateCTM(cgCtx, 0, box.size.width)
		cgContextRotateCTM(cgCtx, -math.Pi/2)
	case 180:
		cgContextTranslateCTM(cgCtx, box.size.width, box.size.height)
		cgContextRotateCTM(cgCtx, math.Pi)
	case 270:
		cgContextTranslateCTM(cgCtx, box.size.height, 0)
		cgContextRotateCTM(cgCtx, math.Pi/2)
	}
	cgContextTranslateCTM(cgCtx, -box.origin.x, -box.origin.y)
	cgContextDrawPDFPage(cgCtx, page)
	cgContextRestoreGState(cgCtx)

	// Copy bitmap buffer into Go image.RGBA
	rawData := cgBitmapContextGetData(cgCtx)
	if rawData == nil {
		return nil, fmt.Errorf("failed to get bitmap data pointer")
	}

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	srcSlice := unsafe.Slice((*byte)(rawData), w*h*4)

	// CGBitmapContext data already has row 0 at the top of the image buffer.
	// Copy directly without vertical flipping so the page is right-side up.
	copy(img.Pix, srcSlice)

	return img, nil
}
