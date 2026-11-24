package pdfsign

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"sync"
)

const (
	defaultBitmapCacheMaxBytes = 64 << 20 // 64 MiB uncompressed RGBA budget
	defaultRenderMaxDimPx      = 4096
	defaultRenderMaxPixels     = 16_777_216 // 16 MP (~64 MiB RGBA)
	defaultHiDPIFactor         = 2.0
	maxAdaptiveActualScale     = 3.0
)

// PageDim represents the width and height of a PDF page in points (72 DPI).
type PageDim struct {
	Width  float64
	Height float64
}

// PageRenderer renders a page of a PDF document to an image.Image.
type PageRenderer interface {
	// PageCount returns the total number of pages in the PDF.
	PageCount(ctx context.Context, pdfPath string) (int, error)
	// PageDimensions returns the width and height of the 1-based page in points (72 DPI).
	PageDimensions(ctx context.Context, pdfPath string, pageNum int) (width, height float64, err error)
	// RenderPage renders the 1-based page at the given scale factor (e.g. 1.0 = 72 DPI, 2.0 = 144 DPI).
	RenderPage(ctx context.Context, pdfPath string, pageNum int, scale float64) (image.Image, error)
}

// BatchDimensionRenderer is an optional interface implemented by renderers that can
// inspect all page dimensions in a single pass instead of re-opening the PDF per page.
type BatchDimensionRenderer interface {
	AllPageDimensions(ctx context.Context, pdfPath string) ([]PageDim, error)
}

// AllPageDimensions reads every page's visual dimensions in points, using a single-pass
// batch query whenever the renderer supports BatchDimensionRenderer.
func AllPageDimensions(ctx context.Context, r PageRenderer, pdfPath string) ([]PageDim, error) {
	if r == nil {
		return nil, ErrNoPreviewEngine
	}
	if br, ok := r.(BatchDimensionRenderer); ok {
		return br.AllPageDimensions(ctx, pdfPath)
	}
	cnt, err := r.PageCount(ctx, pdfPath)
	if err != nil {
		return nil, err
	}
	if cnt <= 0 {
		return nil, nil
	}
	dims := make([]PageDim, cnt)
	for p := 1; p <= cnt; p++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		w, h, err := r.PageDimensions(ctx, pdfPath, p)
		if err != nil {
			return nil, err
		}
		dims[p-1] = PageDim{Width: w, Height: h}
	}
	return dims, nil
}

// computeAdaptiveRenderScale returns the effective raster scale factor (relative to 72 DPI)
// using 2.0x HiDPI at normal reading zoom (<=1.5x), tapering at extreme zoom so peak
// bitmap size never exceeds maxDimPx on either edge or maxPixels total area.
func computeAdaptiveRenderScale(scale, visW, visH float64, maxDimPx int, maxPixels int64) float64 {
	if scale <= 0 {
		scale = 1.0
	}
	actual := scale * defaultHiDPIFactor
	capScale := math.Max(scale, maxAdaptiveActualScale)
	if actual > capScale {
		actual = capScale
	}
	if visW > 0 && visH > 0 {
		if maxDimPx > 0 {
			maxPt := math.Max(visW, visH)
			if maxPt*actual > float64(maxDimPx) {
				actual = float64(maxDimPx) / maxPt
			}
		}
		if maxPixels > 0 {
			area := visW * visH * actual * actual
			if area > float64(maxPixels) {
				actual = math.Sqrt(float64(maxPixels) / (visW * visH))
			}
		}
	}
	if actual <= 0 {
		actual = 0.5
	}
	return actual
}

// PageCacheKey identifies a rendered page bitmap or encoded page PNG.
type PageCacheKey struct {
	PDFPath   string
	PageNum   int
	Scale1000 int // scale * 1000 rounded to integer
}

// PageBitmapCache is a thread-safe LRU cache for rendered PDF page bitmaps
// bounded by both entry count and estimated uncompressed pixel bytes.
type PageBitmapCache struct {
	mu        sync.Mutex
	capacity  int
	maxBytes  int64
	curBytes  int64
	items     map[PageCacheKey]*list.Element
	evictList *list.List
}

type cacheEntry struct {
	key   PageCacheKey
	img   image.Image
	bytes int64
}

func estimateImageBytes(img image.Image) int64 {
	if img == nil {
		return 0
	}
	b := img.Bounds()
	w, h := int64(b.Dx()), int64(b.Dy())
	if w <= 0 || h <= 0 {
		return 1
	}
	return w * h * 4
}

// QuantizeRenderScale snaps continuous zoom to 0.25 increments within [0.25, 3.0]
// so continuous pinch/scroll zoom shares cached raster scales instead of thrashing.
func QuantizeRenderScale(zoom float64) float64 {
	if zoom <= 0 {
		return 1.0
	}
	q := math.Round(zoom*4) / 4
	if q < 0.25 {
		return 0.25
	}
	if q > 3.0 {
		return 3.0
	}
	return q
}

// NewPageBitmapCache creates an LRU cache with the given entry capacity and a default 64 MiB budget.
func NewPageBitmapCache(capacity int) *PageBitmapCache {
	return NewPageBitmapCacheWithBudget(capacity, defaultBitmapCacheMaxBytes)
}

// NewPageBitmapCacheWithBudget creates an LRU cache bounded by both entry count and pixel bytes.
func NewPageBitmapCacheWithBudget(capacity int, maxBytes int64) *PageBitmapCache {
	if capacity <= 0 {
		capacity = 20
	}
	if maxBytes <= 0 {
		maxBytes = defaultBitmapCacheMaxBytes
	}
	return &PageBitmapCache{
		capacity:  capacity,
		maxBytes:  maxBytes,
		items:     make(map[PageCacheKey]*list.Element, capacity),
		evictList: list.New(),
	}
}

// Get retrieves a cached page image if present.
func (c *PageBitmapCache) Get(pdfPath string, pageNum int, scale float64) (image.Image, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := PageCacheKey{
		PDFPath:   pdfPath,
		PageNum:   pageNum,
		Scale1000: int(math.Round(scale * 1000)),
	}

	if elem, ok := c.items[key]; ok {
		c.evictList.MoveToFront(elem)
		return elem.Value.(*cacheEntry).img, true
	}
	return nil, false
}

// GetNearest returns any available cached image for the given PDF and page number,
// useful for smooth continuous zooming before a new raster completes.
func (c *PageBitmapCache) GetNearest(pdfPath string, pageNum int) (image.Image, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for elem := c.evictList.Front(); elem != nil; elem = elem.Next() {
		entry := elem.Value.(*cacheEntry)
		if entry.key.PDFPath == pdfPath && entry.key.PageNum == pageNum {
			return entry.img, true
		}
	}
	return nil, false
}

// Put stores a rendered page image in the cache.
func (c *PageBitmapCache) Put(pdfPath string, pageNum int, scale float64, img image.Image) {
	if img == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	key := PageCacheKey{
		PDFPath:   pdfPath,
		PageNum:   pageNum,
		Scale1000: int(math.Round(scale * 1000)),
	}
	sz := estimateImageBytes(img)

	if elem, ok := c.items[key]; ok {
		c.evictList.MoveToFront(elem)
		entry := elem.Value.(*cacheEntry)
		c.curBytes -= entry.bytes
		entry.img = img
		entry.bytes = sz
		c.curBytes += sz
		c.evictLocked(elem)
		return
	}

	elem := c.evictList.PushFront(&cacheEntry{key: key, img: img, bytes: sz})
	c.items[key] = elem
	c.curBytes += sz
	c.evictLocked(elem)
}

func (c *PageBitmapCache) evictLocked(keep *list.Element) {
	for c.evictList.Len() > 1 && (c.evictList.Len() > c.capacity || c.curBytes > c.maxBytes) {
		oldest := c.evictList.Back()
		if oldest == nil || oldest == keep {
			break
		}
		entry := oldest.Value.(*cacheEntry)
		c.evictList.Remove(oldest)
		delete(c.items, entry.key)
		c.curBytes -= entry.bytes
		if c.curBytes < 0 {
			c.curBytes = 0
		}
	}
}

// Clear invalidates all cached pages.
func (c *PageBitmapCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[PageCacheKey]*list.Element, c.capacity)
	c.evictList.Init()
	c.curBytes = 0
}

// ClearForPath removes cached entries for a specific PDF.
func (c *PageBitmapCache) ClearForPath(pdfPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var next *list.Element
	for elem := c.evictList.Front(); elem != nil; elem = next {
		next = elem.Next()
		entry := elem.Value.(*cacheEntry)
		if entry.key.PDFPath == pdfPath {
			c.evictList.Remove(elem)
			delete(c.items, entry.key)
			c.curBytes -= entry.bytes
			if c.curBytes < 0 {
				c.curBytes = 0
			}
		}
	}
}

// Bytes returns the estimated uncompressed bytes currently cached.
func (c *PageBitmapCache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curBytes
}

// PageBytesCache is a thread-safe, byte-budgeted LRU cache storing compressed PNG page bytes.
type PageBytesCache struct {
	mu        sync.Mutex
	maxBytes  int64
	curBytes  int64
	items     map[PageCacheKey]*list.Element
	evictList *list.List
}

type byteCacheEntry struct {
	key  PageCacheKey
	data []byte
}

// NewPageBytesCache creates a compressed-byte LRU cache with the given byte budget.
func NewPageBytesCache(maxBytes int64) *PageBytesCache {
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	return &PageBytesCache{
		maxBytes:  maxBytes,
		items:     make(map[PageCacheKey]*list.Element, 64),
		evictList: list.New(),
	}
}

// Get retrieves cached PNG bytes if present.
func (c *PageBytesCache) Get(pdfPath string, pageNum int, scale float64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := PageCacheKey{
		PDFPath:   pdfPath,
		PageNum:   pageNum,
		Scale1000: int(math.Round(scale * 1000)),
	}
	if elem, ok := c.items[key]; ok {
		c.evictList.MoveToFront(elem)
		return elem.Value.(*byteCacheEntry).data, true
	}
	return nil, false
}

// Put stores compressed PNG bytes, evicting LRU entries to enforce maxBytes.
func (c *PageBytesCache) Put(pdfPath string, pageNum int, scale float64, data []byte) {
	if len(data) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	key := PageCacheKey{
		PDFPath:   pdfPath,
		PageNum:   pageNum,
		Scale1000: int(math.Round(scale * 1000)),
	}
	sz := int64(len(data))
	if sz > c.maxBytes {
		return
	}

	if elem, ok := c.items[key]; ok {
		c.evictList.MoveToFront(elem)
		entry := elem.Value.(*byteCacheEntry)
		c.curBytes -= int64(len(entry.data))
		entry.data = data
		c.curBytes += sz
		c.evictLocked(elem)
		return
	}

	elem := c.evictList.PushFront(&byteCacheEntry{key: key, data: data})
	c.items[key] = elem
	c.curBytes += sz
	c.evictLocked(elem)
}

func (c *PageBytesCache) evictLocked(keep *list.Element) {
	for c.evictList.Len() > 1 && c.curBytes > c.maxBytes {
		oldest := c.evictList.Back()
		if oldest == nil || oldest == keep {
			break
		}
		entry := oldest.Value.(*byteCacheEntry)
		c.evictList.Remove(oldest)
		delete(c.items, entry.key)
		c.curBytes -= int64(len(entry.data))
		if c.curBytes < 0 {
			c.curBytes = 0
		}
	}
}

// Clear removes all cached byte entries.
func (c *PageBytesCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[PageCacheKey]*list.Element, 64)
	c.evictList.Init()
	c.curBytes = 0
}

// ClearForPath removes cached byte entries for a specific PDF path.
func (c *PageBytesCache) ClearForPath(pdfPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var next *list.Element
	for elem := c.evictList.Front(); elem != nil; elem = next {
		next = elem.Next()
		entry := elem.Value.(*byteCacheEntry)
		if entry.key.PDFPath == pdfPath {
			c.evictList.Remove(elem)
			delete(c.items, entry.key)
			c.curBytes -= int64(len(entry.data))
			if c.curBytes < 0 {
				c.curBytes = 0
			}
		}
	}
}

// Bytes returns the total compressed bytes currently held in cache.
func (c *PageBytesCache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curBytes
}

// DefaultRenderer returns the platform-recommended PageRenderer.
func DefaultRenderer() PageRenderer {
	return defaultPlatformRenderer()
}

// RendererConfig supplies runtime configuration for external preview engines (e.g. pdfium.wasm).
type RendererConfig struct {
	WASMPath string
	CacheDir string
}

// ConfigurableRenderer is optionally implemented by renderers that accept runtime configuration and own resources.
type ConfigurableRenderer interface {
	Configure(cfg RendererConfig)
	Close() error
}

// ErrPageOutOfRange is returned when a requested page is not in the PDF.
var ErrPageOutOfRange = fmt.Errorf("pdf page number out of range")

// ErrPDFiumMissing indicates that pdfium.wasm is not configured or not found on disk.
var ErrPDFiumMissing = errors.New("pdfium.wasm is not configured or missing")

// ErrPDFiumInvalid indicates that the configured pdfium.wasm failed validation or failed to initialize.
var ErrPDFiumInvalid = errors.New("pdfium.wasm is invalid or failed to initialize")

// ErrNoPreviewEngine indicates that no PDF rendering engine is available on this platform.
var ErrNoPreviewEngine = errors.New("no PDF preview engine is available")

// ErrPasswordRequired indicates that the PDF requires a password to open or render.
var ErrPasswordRequired = errors.New("pdf password is required or incorrect")
