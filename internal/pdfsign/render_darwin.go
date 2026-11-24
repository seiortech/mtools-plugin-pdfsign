//go:build darwin

package pdfsign

import (
	"context"
	"fmt"
	"image"
	"math"
	"os"
	"runtime"
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

	cgDataProviderCreateWithFilename func(filename string) uintptr
	cgDataProviderCreateWithData     func(info unsafe.Pointer, data unsafe.Pointer, size uintptr, releaseData unsafe.Pointer) uintptr
	cgPDFDocumentCreateWithProvider  func(provider uintptr) uintptr
	cgPDFDocumentGetNumberOfPages    func(doc uintptr) uintptr
	cgPDFDocumentGetPage             func(doc uintptr, pageNum uintptr) uintptr
	cgPDFPageGetBoxRect              func(page uintptr, box int32) cgRect
	cgPDFPageGetRotationAngle        func(page uintptr) int32
	cgColorSpaceCreateDeviceRGB      func() uintptr
	cgBitmapContextCreate            func(data unsafe.Pointer, width, height, bitsPerComp, bytesPerRow, space uintptr, bitmapInfo uint32) uintptr
	cgBitmapContextGetData           func(ctx uintptr) unsafe.Pointer
	cgContextSetRGBFillColor         func(ctx uintptr, r, g, b, a float64)
	cgContextFillRect                func(ctx uintptr, rect cgRect)
	cgContextSaveGState              func(ctx uintptr)
	cgContextTranslateCTM            func(ctx uintptr, tx, ty float64)
	cgContextScaleCTM                func(ctx uintptr, sx, sy float64)
	cgContextRotateCTM               func(ctx uintptr, angle float64)
	cgContextDrawPDFPage             func(ctx uintptr, page uintptr)
	cgContextRestoreGState           func(ctx uintptr)
	cgContextSetAllowsAntialiasing   func(ctx uintptr, allows bool)
	cgContextSetShouldAntialias      func(ctx uintptr, should bool)
	cgContextSetShouldSmoothFonts    func(ctx uintptr, should bool)
	cgContextSetInterpolationQuality func(ctx uintptr, quality int32)
	cgContextRelease                 func(ctx uintptr)
	cgColorSpaceRelease              func(space uintptr)
	cgPDFDocumentRelease             func(doc uintptr)
	cgDataProviderRelease            func(provider uintptr)
)

const (
	kCGPDFMediaBox = 0
	kCGPDFCropBox  = 1

	kCGImageAlphaPremultipliedLast = 1
	kCGBitmapByteOrder32Big        = 4 << 12

	kCGInterpolationHigh = 3

	maxCGMemoryFallbackBytes = 16 << 20 // 16 MiB cap for in-memory fallback reads
)

func initCoreGraphics() error {
	darwinInitOnce.Do(func() {
		cgLib, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			darwinInitErr = fmt.Errorf("failed to dlopen CoreGraphics: %w", err)
			return
		}

		purego.RegisterLibFunc(&cgDataProviderCreateWithFilename, cgLib, "CGDataProviderCreateWithFilename")
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
// It streams PDFs via CGDataProviderCreateWithFilename so large files never copy into Go heap.
type DarwinCoreGraphicsRenderer struct {
	fallback PageRenderer
}

func defaultPlatformRenderer() PageRenderer {
	return &DarwinCoreGraphicsRenderer{
		fallback: &FallbackRenderer{},
	}
}

// Close releases renderer resources.
func (r *DarwinCoreGraphicsRenderer) Close() error {
	return nil
}

func openCGDoc(pdfPath string) (doc uintptr, cleanup func(), err error) {
	info, statErr := os.Stat(pdfPath)
	if statErr != nil {
		return 0, nil, fmt.Errorf("stat pdf %s: %w", pdfPath, statErr)
	}
	if info.IsDir() || info.Size() <= 0 {
		return 0, nil, fmt.Errorf("empty or invalid pdf %s", pdfPath)
	}

	if cgDataProviderCreateWithFilename != nil {
		if provider := cgDataProviderCreateWithFilename(pdfPath); provider != 0 {
			if d := cgPDFDocumentCreateWithProvider(provider); d != 0 {
				return d, func() {
					cgPDFDocumentRelease(d)
					cgDataProviderRelease(provider)
				}, nil
			}
			cgDataProviderRelease(provider)
		}
	}

	if info.Size() > maxCGMemoryFallbackBytes {
		return 0, nil, fmt.Errorf("failed to open pdf document %s via CoreGraphics file provider", pdfPath)
	}
	data, readErr := os.ReadFile(pdfPath)
	if readErr != nil {
		return 0, nil, fmt.Errorf("read pdf %s: %w", pdfPath, readErr)
	}
	if len(data) == 0 {
		return 0, nil, fmt.Errorf("empty pdf %s", pdfPath)
	}

	provider := cgDataProviderCreateWithData(nil, unsafe.Pointer(&data[0]), uintptr(len(data)), nil)
	if provider == 0 {
		runtime.KeepAlive(data)
		return 0, nil, fmt.Errorf("failed to create CoreGraphics data provider")
	}
	d := cgPDFDocumentCreateWithProvider(provider)
	if d == 0 {
		cgDataProviderRelease(provider)
		runtime.KeepAlive(data)
		return 0, nil, fmt.Errorf("failed to create CoreGraphics pdf document")
	}
	return d, func() {
		cgPDFDocumentRelease(d)
		cgDataProviderRelease(provider)
		runtime.KeepAlive(data)
	}, nil
}

func cgPageVisualBox(page uintptr) (box cgRect, rot int, visW, visH float64) {
	box = cgPDFPageGetBoxRect(page, kCGPDFCropBox)
	if box.size.width <= 0 || box.size.height <= 0 {
		box = cgPDFPageGetBoxRect(page, kCGPDFMediaBox)
	}
	if cgPDFPageGetRotationAngle != nil {
		rot = ((int(cgPDFPageGetRotationAngle(page)) % 360) + 360) % 360
	}
	visW, visH = box.size.width, box.size.height
	if rot == 90 || rot == 270 {
		visW, visH = visH, visW
	}
	return box, rot, visW, visH
}

func (r *DarwinCoreGraphicsRenderer) PageCount(ctx context.Context, pdfPath string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := initCoreGraphics(); err != nil {
		return r.fallback.PageCount(ctx, pdfPath)
	}

	doc, cleanup, err := openCGDoc(pdfPath)
	if err != nil {
		return r.fallback.PageCount(ctx, pdfPath)
	}
	defer cleanup()

	return int(cgPDFDocumentGetNumberOfPages(doc)), nil
}

// AllPageDimensions returns all page visual dimensions in points in a single parse pass.
func (r *DarwinCoreGraphicsRenderer) AllPageDimensions(ctx context.Context, pdfPath string) ([]PageDim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f, err := os.Open(pdfPath); err == nil {
		dims, dimErr := api.PageDims(ctx, f, model.NewStatelessConfiguration())
		_ = f.Close()
		if dimErr == nil && len(dims) > 0 {
			out := make([]PageDim, len(dims))
			for i, d := range dims {
				out[i] = PageDim{Width: d.Width, Height: d.Height}
			}
			return out, nil
		}
	}

	if err := initCoreGraphics(); err != nil {
		return AllPageDimensions(ctx, r.fallback, pdfPath)
	}

	doc, cleanup, err := openCGDoc(pdfPath)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	total := int(cgPDFDocumentGetNumberOfPages(doc))
	if total <= 0 {
		return nil, nil
	}
	out := make([]PageDim, total)
	for p := 1; p <= total; p++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page := cgPDFDocumentGetPage(doc, uintptr(p))
		if page == 0 {
			return nil, fmt.Errorf("failed to get page %d", p)
		}
		_, _, visW, visH := cgPageVisualBox(page)
		out[p-1] = PageDim{Width: visW, Height: visH}
	}
	return out, nil
}

func (r *DarwinCoreGraphicsRenderer) PageDimensions(ctx context.Context, pdfPath string, pageNum int) (float64, float64, error) {
	if pageNum < 1 {
		return 0, 0, ErrPageOutOfRange
	}
	dims, err := r.AllPageDimensions(ctx, pdfPath)
	if err != nil {
		return 0, 0, err
	}
	if pageNum > len(dims) {
		return 0, 0, ErrPageOutOfRange
	}
	return dims[pageNum-1].Width, dims[pageNum-1].Height, nil
}

func (r *DarwinCoreGraphicsRenderer) RenderPage(ctx context.Context, pdfPath string, pageNum int, scale float64) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scale <= 0 {
		scale = 1.0
	}

	if err := initCoreGraphics(); err != nil {
		return r.fallback.RenderPage(ctx, pdfPath, pageNum, scale)
	}

	doc, cleanup, err := openCGDoc(pdfPath)
	if err != nil {
		if _, statErr := os.Stat(pdfPath); statErr != nil {
			return nil, err
		}
		return r.fallback.RenderPage(ctx, pdfPath, pageNum, scale)
	}
	defer cleanup()

	total := int(cgPDFDocumentGetNumberOfPages(doc))
	if pageNum < 1 || pageNum > total {
		return nil, ErrPageOutOfRange
	}

	page := cgPDFDocumentGetPage(doc, uintptr(pageNum))
	if page == 0 {
		return nil, fmt.Errorf("failed to get page %d", pageNum)
	}

	box, rot, visW, visH := cgPageVisualBox(page)
	actualScale := computeAdaptiveRenderScale(scale, visW, visH, defaultRenderMaxDimPx, defaultRenderMaxPixels)

	w := int(math.Ceil(visW * actualScale))
	h := int(math.Ceil(visH * actualScale))
	if w > defaultRenderMaxDimPx {
		w = defaultRenderMaxDimPx
	}
	if h > defaultRenderMaxDimPx {
		h = defaultRenderMaxDimPx
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid page dimensions: %vx%v", w, h)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
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

	cgContextSetRGBFillColor(cgCtx, 1.0, 1.0, 1.0, 1.0)
	cgContextFillRect(cgCtx, cgRect{origin: cgPoint{0, 0}, size: cgSize{float64(w), float64(h)}})

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

	rawData := cgBitmapContextGetData(cgCtx)
	if rawData == nil {
		return nil, fmt.Errorf("failed to get bitmap data pointer")
	}

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	srcSlice := unsafe.Slice((*byte)(rawData), w*h*4)
	copy(img.Pix, srcSlice)

	return img, nil
}

var (
	_ PageRenderer           = (*DarwinCoreGraphicsRenderer)(nil)
	_ BatchDimensionRenderer = (*DarwinCoreGraphicsRenderer)(nil)
)
