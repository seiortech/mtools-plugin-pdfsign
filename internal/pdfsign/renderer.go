package pdfsign

import (
	"container/list"
	"context"
	"fmt"
	"image"
	"math"
	"sync"
)

// PageRenderer renders a page of a PDF document to an image.Image.
type PageRenderer interface {
	// PageCount returns the total number of pages in the PDF.
	PageCount(ctx context.Context, pdfPath string) (int, error)
	// PageDimensions returns the width and height of the 1-based page in points (72 DPI).
	PageDimensions(ctx context.Context, pdfPath string, pageNum int) (width, height float64, err error)
	// RenderPage renders the 1-based page at the given scale factor (e.g. 1.0 = 72 DPI, 2.0 = 144 DPI).
	RenderPage(ctx context.Context, pdfPath string, pageNum int, scale float64) (image.Image, error)
}

// PageCacheKey identifies a rendered page bitmap.
type PageCacheKey struct {
	PDFPath   string
	PageNum   int
	Scale1000 int // scale * 1000 rounded to integer
}

// PageBitmapCache is a thread-safe LRU cache for rendered PDF page bitmaps.
type PageBitmapCache struct {
	mu       sync.Mutex
	capacity int
	items    map[PageCacheKey]*list.Element
	evictList *list.List
}

type cacheEntry struct {
	key PageCacheKey
	img image.Image
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

// NewPageBitmapCache creates an LRU cache with the given capacity (maximum number of pages).
func NewPageBitmapCache(capacity int) *PageBitmapCache {
	if capacity <= 0 {
		capacity = 20
	}
	return &PageBitmapCache{
		capacity:  capacity,
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

	if elem, ok := c.items[key]; ok {
		c.evictList.MoveToFront(elem)
		entry := elem.Value.(*cacheEntry)
		entry.img = img
		return
	}

	for c.evictList.Len() >= c.capacity {
		oldest := c.evictList.Back()
		if oldest == nil {
			break
		}
		c.evictList.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
	}

	elem := c.evictList.PushFront(&cacheEntry{key: key, img: img})
	c.items[key] = elem
}

// Clear invalidates all cached pages.
func (c *PageBitmapCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[PageCacheKey]*list.Element, c.capacity)
	c.evictList.Init()
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
		}
	}
}

// DefaultRenderer returns the platform-recommended PageRenderer.
func DefaultRenderer() PageRenderer {
	return defaultPlatformRenderer()
}

// ErrPageOutOfRange is returned when a requested page is not in the PDF.
var ErrPageOutOfRange = fmt.Errorf("pdf page number out of range")
