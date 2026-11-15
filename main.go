package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seiortech/mtools-plugin-pdfsign/internal/pdfsign"
	"github.com/seiortech/mtools-sdk/pluginapi"
	"github.com/seiortech/mtools-sdk/ui/i18n"
)

//go:embed locales/*.json
var localesFS embed.FS

const (
	defaultPageW     = 595.0
	defaultPageH     = 842.0
	studioCanvasW    = 560
	studioCanvasH    = 240
	studioModeDraw   = 0
	studioModePhoto  = 1
	maxZoomFactor    = 3.0
	minZoomFactor    = 0.25
	defaultRailWidth = 280
)

type pageSize struct {
	W float64
	H float64
}

type pageAssetKey struct {
	docGen    uint64
	pageNum   int
	scale1000 int
}

type studioState struct {
	open            bool
	mode            int // 0=Draw, 1=Photo
	inkIdx          int // 0=Black, 1=Blue, 2=Red
	eraser          bool
	penWidth        float64
	strokes         []pdfsign.Stroke
	drawSeq         int
	drawPreviewRef  string
	drawPreviewFile string

	photoPath        string
	rawPhoto         image.Image
	photoThumb       image.Image
	threshold        float64
	contrast         float64
	photoInk         pdfsign.InkColorMode
	photoResult      *image.NRGBA
	photoSeq         int
	photoPreviewRef  string
	photoPreviewFile string
	photoDebounce    *time.Timer
	photoGen         uint64
}

type pdfSignServer struct {
	mu       sync.Mutex
	rev      uint64
	dataDir  string
	vault    *pdfsign.SignatureVault
	history  *pdfsign.HistoryStore
	renderer pdfsign.PageRenderer
	catalog  *i18n.Catalog
	lang     i18n.Lang
	sess     *pluginapi.UISession

	// Workspace document & viewport state
	pdfPath        string
	docGen         uint64
	pageCount      int
	currentPage    int
	pageSizes      []pageSize
	zoom           float64
	fitZoom        float64
	viewportW      float64
	viewportH      float64
	scrollY        float64
	targetScrollY  float64
	scrollSeq      uint64
	railCollapsed  bool
	railWidth      float32
	lockAspect     bool
	statusMsg      string
	errMsg         string
	openAfterSign  bool

	// Page bitmap + asset cache
	pageCache       *pdfsign.PageBitmapCache
	pageAssets      map[pageAssetKey]string // asset ref ("asset:...")
	pageAssetFiles  map[string]string       // asset ref -> basename
	renderSem       chan struct{}
	inFlightRenders map[pageAssetKey]bool
	zoomDebounce    *time.Timer
	assetSeq        int

	// Vault assets & placements
	vaultAssets    map[string]string // vault id -> asset ref
	placements     []pdfsign.PlacedSignature
	placedAssets   map[string]string // placement id -> asset ref
	placedFiles    map[string]string // placement id -> basename
	activeID       string
	nextSigSeq     int

	// Export state
	exporting   bool
	exportDone  bool
	exportPath  string
	exportErr   string
	exportCancel context.CancelFunc
	exportGen   uint64

	// Studio modal state
	studio studioState
}

type rawPlacement struct {
	ID        string  `json:"id"`
	Page      int     `json:"page"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Width     float64 `json:"width"`
	Height    float64 `json:"height"`
	Rotation  float64 `json:"rotation"`
	Opacity   float64 `json:"opacity"`
	Base64PNG string  `json:"base64_png"`
}

func newPDFSignServer() *pdfSignServer {
	cat, _ := i18n.NewCatalogFromFS(localesFS, "locales")
	return &pdfSignServer{
		renderer:        pdfsign.DefaultRenderer(),
		catalog:         cat,
		lang:            i18n.LangEN,
		currentPage:     1,
		zoom:            1.0,
		fitZoom:         1.0,
		railWidth:       defaultRailWidth,
		lockAspect:      true,
		pageCache:       pdfsign.NewPageBitmapCache(48),
		pageAssets:      make(map[pageAssetKey]string),
		pageAssetFiles:  make(map[string]string),
		renderSem:       make(chan struct{}, 2),
		inFlightRenders: make(map[pageAssetKey]bool),
		vaultAssets:     make(map[string]string),
		placedAssets:    make(map[string]string),
		placedFiles:     make(map[string]string),
		studio: studioState{
			penWidth:  3.0,
			threshold: 0.80,
			contrast:  1.5,
			photoInk:  pdfsign.InkBlack,
		},
	}
}

func (s *pdfSignServer) T(key string) string {
	if s.catalog == nil {
		return key
	}
	return s.catalog.T(s.lang, key)
}

func (s *pdfSignServer) Tf(key string, args ...any) string {
	if s.catalog == nil {
		return fmt.Sprintf(key, args...)
	}
	return s.catalog.Tf(s.lang, key, args...)
}

func (s *pdfSignServer) Inspect(ctx context.Context, p pluginapi.InspectParams) (pluginapi.InspectResult, error) {
	res := pluginapi.InspectResult{
		Fields:       make(map[string]pluginapi.InspectFieldUpdate),
		Metadata:     make(map[string]string),
		HelperStatus: make(map[string]pluginapi.HelperStatus),
	}
	if files, ok := p.Inputs["input_file"]; ok && len(files) > 0 && files[0] != "" {
		if count, err := s.renderer.PageCount(ctx, files[0]); err == nil {
			res.Fields["input_file"] = pluginapi.InspectFieldUpdate{
				Info: fmt.Sprintf("%d pages", count),
			}
		}
	}
	return res, nil
}

func (s *pdfSignServer) Run(ctx context.Context, p pluginapi.RunParams, emit func(pluginapi.Progress) error) (pluginapi.RunResult, error) {
	if p.Tool != "pdfsign" {
		return pluginapi.RunResult{}, errors.New("unsupported tool")
	}
	switch p.Action {
	case "sign":
		inFiles := p.Inputs["input_file"]
		if len(inFiles) == 0 || inFiles[0] == "" {
			return pluginapi.RunResult{}, errors.New("missing input PDF")
		}
		inFile := inFiles[0]

		outFiles := p.Inputs["output_file"]
		if len(outFiles) == 0 || outFiles[0] == "" {
			return pluginapi.RunResult{}, errors.New("missing output file")
		}
		outFile := outFiles[0]

		var placements []pdfsign.PlacedSignature
		if rawJSON, ok := p.Inputs["placements"]; ok && len(rawJSON) > 0 && rawJSON[0] != "" {
			var rawList []rawPlacement
			if err := json.Unmarshal([]byte(rawJSON[0]), &rawList); err == nil {
				for _, r := range rawList {
					var img image.Image
					if r.Base64PNG != "" {
						data, err := base64.StdEncoding.DecodeString(r.Base64PNG)
						if err == nil {
							img, _ = png.Decode(bytes.NewReader(data))
						}
					}
					placements = append(placements, pdfsign.PlacedSignature{
						ID:       r.ID,
						Page:     r.Page,
						X:        r.X,
						Y:        r.Y,
						Width:    r.Width,
						Height:   r.Height,
						Rotation: r.Rotation,
						Opacity:  r.Opacity,
						Crop:     pdfsign.DefaultNormalizedRect(),
						InkMode:  pdfsign.InkOriginal,
						Image:    img,
					})
				}
			}
		}

		_ = emit(pluginapi.Progress{
			JobID:   p.JobID,
			Done:    10,
			Total:   100,
			Unit:    "%",
			Message: "Signing PDF document...",
			LogLine: fmt.Sprintf("Signing %s with %d placement(s)", filepath.Base(inFile), len(placements)),
		})

		req := pdfsign.SignRequest{
			InFile:     inFile,
			OutFile:    outFile,
			Signatures: placements,
		}
		err := pdfsign.SignPDF(ctx, req, func(done float64, msg string) {
			_ = emit(pluginapi.Progress{
				JobID:   p.JobID,
				Done:    done * 100,
				Total:   100,
				Unit:    "%",
				Message: msg,
				LogLine: msg,
			})
		})
		if err != nil {
			return pluginapi.RunResult{}, err
		}

		return pluginapi.RunResult{
			JobID:   p.JobID,
			Outputs: []string{outFile},
			Summary: fmt.Sprintf("Signed PDF saved to %s", filepath.Base(outFile)),
		}, nil
	default:
		return pluginapi.RunResult{}, fmt.Errorf("unknown action: %s", p.Action)
	}
}

func (s *pdfSignServer) Open(ctx context.Context, p pluginapi.UIOpenParams, sess *pluginapi.UISession) (pluginapi.UITree, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sess = sess
	if p.Lang != "" {
		s.lang = i18n.NormalizeLang(p.Lang)
	}
	if p.Viewport.Width > 0 {
		s.viewportW = float64(p.Viewport.Width)
	}
	if p.Viewport.Height > 0 {
		s.viewportH = float64(p.Viewport.Height)
	}

	dataDir := p.Settings["plugin_data_dir"]
	if dataDir == "" {
		dataDir = filepath.Join(os.TempDir(), "mtools-pdfsign-data")
	}
	s.dataDir = dataDir
	s.vault = pdfsign.NewSignatureVault(dataDir)
	s.history = pdfsign.NewHistoryStore(dataDir)
	s.syncVaultAssetsLocked(sess)
	return s.buildTreeLocked(), nil
}

func (s *pdfSignServer) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.zoomDebounce != nil {
		s.zoomDebounce.Stop()
		s.zoomDebounce = nil
	}
	if s.studio.photoDebounce != nil {
		s.studio.photoDebounce.Stop()
		s.studio.photoDebounce = nil
	}
	if s.exportCancel != nil {
		s.exportCancel()
		s.exportCancel = nil
	}
	return nil
}

func (s *pdfSignServer) Event(ctx context.Context, ev pluginapi.UIEvent, sess *pluginapi.UISession) error {
	s.mu.Lock()
	if sess != nil {
		s.sess = sess
	}
	changed := s.handleEventLocked(ctx, ev, sess)
	var tree pluginapi.UITree
	if changed {
		tree = s.buildTreeLocked()
	}
	s.mu.Unlock()

	if changed && sess != nil {
		return sess.Render(tree)
	}
	return nil
}

func (s *pdfSignServer) pushTree(sess *pluginapi.UISession) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	tree := s.buildTreeLocked()
	s.mu.Unlock()
	_ = sess.Render(tree)
}

func (s *pdfSignServer) handleEventLocked(ctx context.Context, ev pluginapi.UIEvent, sess *pluginapi.UISession) bool {
	switch ev.Kind {
	case pluginapi.UIEventShortcut:
		return s.handleShortcutLocked(ev.Value)

	case pluginapi.UIEventResize:
		if ev.Node == "main_split" {
			if w, err := strconv.ParseFloat(strings.TrimSpace(ev.Value), 32); err == nil && w >= 200 && w <= 460 {
				s.railWidth = float32(w)
				return true
			}
			return false
		}
		if strings.HasPrefix(ev.Node, "placed_") {
			sigID := strings.TrimPrefix(ev.Node, "placed_")
			x, y, w, h, ok := parseResizeRect(ev.Value)
			if !ok {
				return false
			}
			return s.resizePlacementLocked(sigID, x, y, w, h)
		}

	case pluginapi.UIEventDragEnd:
		if strings.HasPrefix(ev.Node, "placed_") {
			sigID := strings.TrimPrefix(ev.Node, "placed_")
			x, y, ok := parseDragCoords(ev.Value)
			if !ok {
				return false
			}
			return s.movePlacementLocked(sigID, x, y)
		}

	case pluginapi.UIEventDrop:
		if strings.HasPrefix(ev.Node, "page_") {
			pageNum, err := strconv.Atoi(strings.TrimPrefix(ev.Node, "page_"))
			if err != nil {
				return false
			}
			return s.handleVaultDropLocked(pageNum, ev.Value, sess)
		}

	case pluginapi.UIEventScroll:
		if ev.Node == "viewport" {
			return s.handleViewportScrollLocked(ev.Value, sess)
		}

	case pluginapi.UIEventZoom:
		if ev.Node == "viewport" {
			if factor, err := strconv.ParseFloat(strings.TrimSpace(ev.Value), 64); err == nil && factor > 0 {
				s.setZoomLocked(s.zoom*factor, sess)
				return true
			}
		}

	case pluginapi.UIEventStroke:
		if ev.Node == "studio_draw_canvas" {
			pts := parseStrokePoints(ev.Value)
			if len(pts) > 0 {
				col := studioInkRGBA(s.studio.inkIdx)
				w := s.studio.penWidth
				if s.studio.eraser {
					w = 14.0
				}
				s.studio.strokes = append(s.studio.strokes, pdfsign.Stroke{
					Points: pts,
					Width:  w,
					Color:  col,
					Eraser: s.studio.eraser,
				})
				s.refreshDrawPreviewLocked(sess)
				return true
			}
		}

	case pluginapi.UIEventChange:
		switch ev.Node {
		case "open_pdf", "empty_open_pdf":
			if ev.Value != "" {
				s.loadPDFLocked(ctx, ev.Value, sess)
				return true
			}
		case "finish_sign":
			if ev.Value != "" {
				s.startExportLocked(ev.Value, sess)
				return true
			}
		case "studio_choose_photo":
			if ev.Value != "" {
				s.loadStudioPhotoLocked(ev.Value, sess)
				return true
			}
		case "studio_threshold":
			if v, err := strconv.ParseFloat(strings.TrimSpace(ev.Value), 64); err == nil {
				if v < 0.2 {
					v = 0.2
				} else if v > 0.98 {
					v = 0.98
				}
				s.studio.threshold = v
				s.schedulePhotoPreviewLocked(sess)
				return true
			}
		}

	case pluginapi.UIEventSelect:
		if ev.Node == "studio_tabs" {
			if ev.Value == "photo" {
				s.studio.mode = studioModePhoto
			} else {
				s.studio.mode = studioModeDraw
			}
			return true
		}

	case pluginapi.UIEventClose:
		if ev.Node == "studio_dlg" {
			s.studio.open = false
			return true
		}

	case pluginapi.UIEventClick:
		return s.handleClickLocked(ev.Node, sess)
	}
	return false
}

func (s *pdfSignServer) handleShortcutLocked(spec string) bool {
	switch strings.ToLower(strings.TrimSpace(spec)) {
	case "escape":
		if s.studio.open {
			s.studio.open = false
			return true
		}
		if s.activeID != "" {
			s.activeID = ""
			return true
		}
	case "l", "cmd+l", "ctrl+l":
		if !s.studio.open && s.pdfPath != "" {
			s.lockAspect = !s.lockAspect
			return true
		}
	}
	return false
}

func (s *pdfSignServer) handleClickLocked(nodeID string, sess *pluginapi.UISession) bool {
	if s.handleStudioClickLocked(nodeID, sess) {
		return true
	}

	switch nodeID {
	case "viewport":
		if s.activeID != "" {
			s.activeID = ""
			return true
		}
		return false
	case "page_prev":
		if s.currentPage > 1 {
			s.jumpToPageLocked(s.currentPage-1, sess)
			return true
		}
		return false
	case "page_next":
		if s.currentPage < s.pageCount {
			s.jumpToPageLocked(s.currentPage+1, sess)
			return true
		}
		return false
	case "zoom_out":
		s.setZoomLocked(s.zoom-0.25, sess)
		return true
	case "zoom_in":
		s.setZoomLocked(s.zoom+0.25, sess)
		return true
	case "zoom_fit":
		s.fitZoomLocked(sess)
		return true
	case "rail_toggle", "rail_expand":
		s.railCollapsed = !s.railCollapsed
		return true
	case "add_sig_top", "vault_draw_btn":
		s.openStudioLocked(studioModeDraw, sess)
		return true
	case "vault_photo_btn":
		s.openStudioLocked(studioModePhoto, sess)
		return true
	case "dismiss_export":
		s.exportDone = false
		s.exportErr = ""
		return true
	case "sel_lock", "insp_lock":
		s.lockAspect = !s.lockAspect
		return true
	case "sel_rot_left":
		return s.rotateActiveLocked(-90, sess)
	case "sel_rot_right", "insp_rot_right":
		return s.rotateActiveLocked(90, sess)
	case "sel_dup", "insp_dup":
		return s.duplicateActiveLocked(sess)
	case "sel_all_pages", "insp_all_pages":
		return s.applyActiveToAllPagesLocked(sess)
	case "sel_del", "insp_del":
		return s.deleteActiveLocked()
	case "insp_ink_orig":
		return s.recolorActiveLocked(pdfsign.InkOriginal, sess)
	case "insp_ink_black":
		return s.recolorActiveLocked(pdfsign.InkBlack, sess)
	case "insp_ink_blue":
		return s.recolorActiveLocked(pdfsign.InkBlue, sess)
	}

	if strings.HasPrefix(nodeID, "page_") {
		// Clicking empty area of a PDF page deselects active signature.
		if _, err := strconv.Atoi(strings.TrimPrefix(nodeID, "page_")); err == nil {
			if s.activeID != "" {
				s.activeID = ""
				return true
			}
			return false
		}
	}
	if strings.HasPrefix(nodeID, "rail_page_") {
		if p, err := strconv.Atoi(strings.TrimPrefix(nodeID, "rail_page_")); err == nil {
			s.jumpToPageLocked(p, sess)
			return true
		}
	}
	if strings.HasPrefix(nodeID, "placed_") {
		id := strings.TrimPrefix(nodeID, "placed_")
		s.activeID = id
		for _, pl := range s.placements {
			if pl.ID == id {
				s.currentPage = pl.Page
				break
			}
		}
		return true
	}
	if strings.HasPrefix(nodeID, "vault_use_") {
		id := strings.TrimPrefix(nodeID, "vault_use_")
		return s.placeVaultItemCenteredLocked(id, s.currentPage, sess)
	}
	if strings.HasPrefix(nodeID, "vault_del_") {
		id := strings.TrimPrefix(nodeID, "vault_del_")
		if s.vault != nil {
			_ = s.vault.Delete(id)
			s.syncVaultAssetsLocked(sess)
			return true
		}
	}
	return false
}

func (s *pdfSignServer) loadPDFLocked(ctx context.Context, pdfPath string, sess *pluginapi.UISession) {
	s.errMsg = ""
	s.statusMsg = ""
	count, err := s.renderer.PageCount(ctx, pdfPath)
	if err != nil {
		s.errMsg = fmt.Sprintf("Failed to open PDF: %v", err)
		return
	}
	if count <= 0 {
		s.errMsg = "PDF has no pages."
		return
	}

	s.docGen++
	s.pdfPath = pdfPath
	s.pageCount = count
	s.currentPage = 1
	s.placements = nil
	s.activeID = ""
	s.exportDone = false
	s.exportErr = ""
	s.exportPath = ""
	s.scrollY = 0
	s.scrollSeq++
	s.targetScrollY = 0

	s.clearPageAssetsLocked(sess)
	s.clearPlacedAssetsLocked(sess)

	sizes := make([]pageSize, count)
	for i := 1; i <= count; i++ {
		w, h, err := s.renderer.PageDimensions(ctx, pdfPath, i)
		if err != nil || w <= 0 || h <= 0 {
			w, h = defaultPageW, defaultPageH
		}
		sizes[i-1] = pageSize{W: w, H: h}
	}
	s.pageSizes = sizes
	s.computeFitZoomLocked()
	s.zoom = s.fitZoom
	s.triggerVisiblePageRendersLocked(sess)
}

func (s *pdfSignServer) computeFitZoomLocked() {
	if len(s.pageSizes) == 0 {
		s.fitZoom = 1.0
		return
	}
	first := s.pageSizes[0]
	railW := float64(s.railWidth)
	if s.railCollapsed {
		railW = 48
	}
	vpW := s.viewportW
	if vpW <= 0 {
		vpW = 900
	}
	availW := vpW - railW - 64
	if availW < 360 {
		availW = 540
	}
	z := availW / first.W
	if z < 0.5 {
		z = 0.5
	} else if z > 1.8 {
		z = 1.8
	}
	s.fitZoom = z
}

func (s *pdfSignServer) fitZoomLocked(sess *pluginapi.UISession) {
	s.computeFitZoomLocked()
	s.setZoomLocked(s.fitZoom, sess)
}

func (s *pdfSignServer) setZoomLocked(z float64, sess *pluginapi.UISession) {
	if z < minZoomFactor {
		z = minZoomFactor
	} else if z > maxZoomFactor {
		z = maxZoomFactor
	}
	if math.Abs(z-s.zoom) < 0.002 {
		return
	}
	s.zoom = z
	if s.pdfPath == "" {
		return
	}
	if s.zoomDebounce != nil {
		s.zoomDebounce.Stop()
	}
	docGen := s.docGen
	s.zoomDebounce = time.AfterFunc(100*time.Millisecond, func() {
		s.mu.Lock()
		if s.docGen != docGen || s.pdfPath == "" {
			s.mu.Unlock()
			return
		}
		s.triggerVisiblePageRendersLocked(sess)
		s.mu.Unlock()
	})
}

func (s *pdfSignServer) pageTopYLocked(pageNum int) float64 {
	const pageGap = 24.0
	const topPad = 24.0
	y := topPad
	z := s.effectiveZoomLocked()
	for i := 1; i < pageNum && i <= len(s.pageSizes); i++ {
		y += s.pageSizes[i-1].H*z + pageGap
	}
	return y
}

func (s *pdfSignServer) jumpToPageLocked(pageNum int, sess *pluginapi.UISession) {
	if s.pageCount == 0 {
		return
	}
	if pageNum < 1 {
		pageNum = 1
	} else if pageNum > s.pageCount {
		pageNum = s.pageCount
	}
	s.currentPage = pageNum
	targetY := s.pageTopYLocked(pageNum) - 24
	if targetY < 0 {
		targetY = 0
	}
	s.scrollSeq++
	s.targetScrollY = targetY
	s.triggerVisiblePageRendersLocked(sess)
}

func (s *pdfSignServer) handleViewportScrollLocked(val string, sess *pluginapi.UISession) bool {
	parts := strings.Split(val, ",")
	if len(parts) < 4 {
		return false
	}
	_, _ = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	sy, e2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	cw, e3 := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
	ch, e4 := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
	if e2 != nil {
		return false
	}
	if e3 == nil && cw > 0 {
		s.viewportW = cw
	}
	if e4 == nil && ch > 0 {
		s.viewportH = ch
	}
	s.scrollY = sy

	if s.pageCount <= 1 || len(s.pageSizes) == 0 {
		return false
	}
	focusY := sy + 200
	if focusY < 0 {
		focusY = 0
	}
	const pageGap = 24.0
	const topPad = 24.0
	z := s.effectiveZoomLocked()
	currY := topPad
	found := 1
	for i := 1; i <= s.pageCount && i <= len(s.pageSizes); i++ {
		ph := s.pageSizes[i-1].H * z
		if focusY >= currY && focusY <= currY+ph+pageGap {
			found = i
			break
		}
		if focusY > currY+ph {
			found = i
		}
		currY += ph + pageGap
	}
	if found != s.currentPage {
		s.currentPage = found
		s.triggerVisiblePageRendersLocked(sess)
		return true
	}
	return false
}

func (s *pdfSignServer) effectiveZoomLocked() float64 {
	if s.zoom <= 0 {
		return 1.0
	}
	return s.zoom
}

func (s *pdfSignServer) triggerVisiblePageRendersLocked(sess *pluginapi.UISession) {
	if s.pdfPath == "" || s.pageCount == 0 {
		return
	}
	scale := pdfsign.QuantizeRenderScale(s.effectiveZoomLocked())
	start := s.currentPage - 2
	if start < 1 {
		start = 1
	}
	end := s.currentPage + 2
	if end > s.pageCount {
		end = s.pageCount
	}
	for p := start; p <= end; p++ {
		s.requestPageRenderLocked(p, scale, sess)
	}
}

func (s *pdfSignServer) requestPageRenderLocked(pageNum int, scale float64, sess *pluginapi.UISession) {
	if pageNum < 1 || pageNum > s.pageCount || s.pdfPath == "" {
		return
	}
	scale1000 := int(math.Round(scale * 1000))
	key := pageAssetKey{docGen: s.docGen, pageNum: pageNum, scale1000: scale1000}
	if _, ok := s.pageAssets[key]; ok {
		return
	}
	if s.inFlightRenders[key] {
		return
	}
	if cachedImg, ok := s.pageCache.Get(s.pdfPath, pageNum, scale); ok {
		s.storeRenderedPageAssetLocked(key, cachedImg, sess)
		return
	}

	s.inFlightRenders[key] = true
	pdfPath := s.pdfPath
	docGen := s.docGen
	renderer := s.renderer
	sem := s.renderSem
	go func() {
		sem <- struct{}{}
		defer func() { <-sem }()

		img, err := renderer.RenderPage(context.Background(), pdfPath, pageNum, scale)
		s.mu.Lock()
		delete(s.inFlightRenders, key)
		if err != nil || img == nil || s.docGen != docGen || s.pdfPath != pdfPath {
			s.mu.Unlock()
			return
		}
		s.pageCache.Put(pdfPath, pageNum, scale, img)
		s.storeRenderedPageAssetLocked(key, img, sess)
		tree := s.buildTreeLocked()
		s.mu.Unlock()

		if sess != nil {
			_ = sess.Render(tree)
		}
	}()
}

func (s *pdfSignServer) storeRenderedPageAssetLocked(key pageAssetKey, img image.Image, sess *pluginapi.UISession) {
	if sess == nil || img == nil {
		return
	}
	s.assetSeq++
	fileName := fmt.Sprintf("page-%d-g%d-s%d-v%d.png", key.pageNum, key.docGen, key.scale1000, s.assetSeq)
	if err := writeAssetPNG(sess.AssetsDir(), fileName, img); err != nil {
		return
	}
	ref := "asset:" + fileName
	s.pageAssets[key] = ref
	s.pageAssetFiles[ref] = fileName
}

func (s *pdfSignServer) pageAssetRefLocked(pageNum int) string {
	scale := pdfsign.QuantizeRenderScale(s.effectiveZoomLocked())
	exactKey := pageAssetKey{
		docGen:    s.docGen,
		pageNum:   pageNum,
		scale1000: int(math.Round(scale * 1000)),
	}
	if ref, ok := s.pageAssets[exactKey]; ok {
		return ref
	}
	bestDist := math.MaxFloat64
	bestRef := ""
	target := int(math.Round(scale * 1000))
	for k, ref := range s.pageAssets {
		if k.docGen != s.docGen || k.pageNum != pageNum {
			continue
		}
		d := math.Abs(float64(k.scale1000 - target))
		if d < bestDist {
			bestDist = d
			bestRef = ref
		}
	}
	return bestRef
}

func (s *pdfSignServer) clearPageAssetsLocked(sess *pluginapi.UISession) {
	if sess != nil {
		dir := sess.AssetsDir()
		for _, fn := range s.pageAssetFiles {
			_ = os.Remove(filepath.Join(dir, fn))
		}
	}
	s.pageAssets = make(map[pageAssetKey]string)
	s.pageAssetFiles = make(map[string]string)
}

func (s *pdfSignServer) clearPlacedAssetsLocked(sess *pluginapi.UISession) {
	if sess != nil {
		dir := sess.AssetsDir()
		for _, fn := range s.placedFiles {
			_ = os.Remove(filepath.Join(dir, fn))
		}
	}
	s.placedAssets = make(map[string]string)
	s.placedFiles = make(map[string]string)
}

func (s *pdfSignServer) pageDimsAtLocked(pageNum int) pageSize {
	if pageNum >= 1 && pageNum <= len(s.pageSizes) {
		return s.pageSizes[pageNum-1]
	}
	return pageSize{W: defaultPageW, H: defaultPageH}
}

func defaultPlacementSize(img image.Image) (float64, float64) {
	b := img.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	if w <= 0 || h <= 0 {
		return 140, 60
	}
	targetW := 140.0
	scale := targetW / w
	targetH := h * scale
	if targetH > 90 {
		targetH = 90
		targetW = w * (targetH / h)
	}
	if targetW < 32 {
		targetW = 32
	}
	if targetH < 18 {
		targetH = 18
	}
	return targetW, targetH
}

func (s *pdfSignServer) placeSignatureAtLocked(img image.Image, sourceID string, page int, x, y float64, sess *pluginapi.UISession) {
	if img == nil || s.pageCount == 0 {
		return
	}
	if page < 1 {
		page = 1
	} else if page > s.pageCount {
		page = s.pageCount
	}
	dims := s.pageDimsAtLocked(page)
	sigW, sigH := defaultPlacementSize(img)
	x = min(max(x, 0), max(dims.W-sigW, 0))
	y = min(max(y, 0), max(dims.H-sigH, 0))

	s.nextSigSeq++
	id := fmt.Sprintf("sig_%d", s.nextSigSeq)
	pl := pdfsign.PlacedSignature{
		ID:       id,
		SourceID: sourceID,
		Page:     page,
		X:        x,
		Y:        y,
		Width:    sigW,
		Height:   sigH,
		Opacity:  1.0,
		Crop:     pdfsign.DefaultNormalizedRect(),
		InkMode:  pdfsign.InkOriginal,
		Image:    img,
	}
	s.placements = append(s.placements, pl)
	s.activeID = id
	s.currentPage = page
	s.refreshPlacementAssetLocked(pl, sess)
}

func (s *pdfSignServer) placeVaultItemCenteredLocked(vaultID string, page int, sess *pluginapi.UISession) bool {
	if s.vault == nil || s.pdfPath == "" {
		return false
	}
	_, pngBytes, err := s.vault.Get(vaultID)
	if err != nil {
		return false
	}
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return false
	}
	dims := s.pageDimsAtLocked(page)
	sigW, sigH := defaultPlacementSize(img)
	x := (dims.W - sigW) / 2
	y := dims.H*0.55 - sigH/2
	s.placeSignatureAtLocked(img, vaultID, page, x, y, sess)
	return true
}

func (s *pdfSignServer) handleVaultDropLocked(pageNum int, raw string, sess *pluginapi.UISession) bool {
	parts := strings.SplitN(raw, "|", 2)
	if len(parts) != 2 {
		return false
	}
	payload := strings.TrimSpace(parts[0])
	if !strings.HasPrefix(payload, "vault:") {
		return false
	}
	vaultID := strings.TrimPrefix(payload, "vault:")
	cx, cy, ok := parseDragCoords(parts[1])
	if !ok || s.vault == nil {
		return false
	}
	_, pngBytes, err := s.vault.Get(vaultID)
	if err != nil {
		return false
	}
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return false
	}
	z := s.effectiveZoomLocked()
	sigW, sigH := defaultPlacementSize(img)
	dropX := cx/z - sigW/2
	dropY := cy/z - sigH/2
	s.placeSignatureAtLocked(img, vaultID, pageNum, dropX, dropY, sess)
	return true
}

func (s *pdfSignServer) refreshPlacementAssetLocked(pl pdfsign.PlacedSignature, sess *pluginapi.UISession) {
	if sess == nil || pl.Image == nil {
		return
	}
	processed := pdfsign.RecolorSignature(pl.Image, pl.InkMode)
	rendered := pdfsign.RenderPlacementImage(processed, pdfsign.PlacementRenderOptions{
		Crop:        pl.Crop,
		RotationDeg: pl.Rotation,
		Opacity:     pl.Opacity,
	})
	s.assetSeq++
	fileName := fmt.Sprintf("placed-%s-v%d.png", pl.ID, s.assetSeq)
	if err := writeAssetPNG(sess.AssetsDir(), fileName, rendered); err != nil {
		return
	}
	if oldFile, ok := s.placedFiles[pl.ID]; ok && oldFile != "" {
		_ = os.Remove(filepath.Join(sess.AssetsDir(), oldFile))
	}
	ref := "asset:" + fileName
	s.placedAssets[pl.ID] = ref
	s.placedFiles[pl.ID] = fileName
}

func (s *pdfSignServer) movePlacementLocked(id string, canvasX, canvasY float64) bool {
	z := s.effectiveZoomLocked()
	px := canvasX / z
	py := canvasY / z
	for i := range s.placements {
		if s.placements[i].ID == id {
			dims := s.pageDimsAtLocked(s.placements[i].Page)
			s.placements[i].X = min(max(px, 0), max(dims.W-s.placements[i].Width, 0))
			s.placements[i].Y = min(max(py, 0), max(dims.H-s.placements[i].Height, 0))
			s.activeID = id
			return true
		}
	}
	return false
}

func (s *pdfSignServer) resizePlacementLocked(id string, canvasX, canvasY, canvasW, canvasH float64) bool {
	z := s.effectiveZoomLocked()
	px := canvasX / z
	py := canvasY / z
	pw := max(canvasW/z, 16)
	ph := max(canvasH/z, 10)
	for i := range s.placements {
		if s.placements[i].ID == id {
			dims := s.pageDimsAtLocked(s.placements[i].Page)
			s.placements[i].X = min(max(px, 0), max(dims.W-pw, 0))
			s.placements[i].Y = min(max(py, 0), max(dims.H-ph, 0))
			s.placements[i].Width = min(pw, dims.W)
			s.placements[i].Height = min(ph, dims.H)
			s.activeID = id
			return true
		}
	}
	return false
}

func (s *pdfSignServer) rotateActiveLocked(deltaDeg float64, sess *pluginapi.UISession) bool {
	for i := range s.placements {
		if s.placements[i].ID == s.activeID {
			s.placements[i].Rotation = math.Mod(s.placements[i].Rotation+deltaDeg+360, 360)
			if math.Abs(math.Mod(math.Abs(deltaDeg), 180)-90) < 1 {
				dims := s.pageDimsAtLocked(s.placements[i].Page)
				cx := s.placements[i].X + s.placements[i].Width/2
				cy := s.placements[i].Y + s.placements[i].Height/2
				s.placements[i].Width, s.placements[i].Height = s.placements[i].Height, s.placements[i].Width
				s.placements[i].X = min(max(cx-s.placements[i].Width/2, 0), max(dims.W-s.placements[i].Width, 0))
				s.placements[i].Y = min(max(cy-s.placements[i].Height/2, 0), max(dims.H-s.placements[i].Height, 0))
			}
			s.refreshPlacementAssetLocked(s.placements[i], sess)
			return true
		}
	}
	return false
}

func (s *pdfSignServer) recolorActiveLocked(mode pdfsign.InkColorMode, sess *pluginapi.UISession) bool {
	for i := range s.placements {
		if s.placements[i].ID == s.activeID {
			s.placements[i].InkMode = mode
			s.refreshPlacementAssetLocked(s.placements[i], sess)
			return true
		}
	}
	return false
}

func (s *pdfSignServer) duplicateActiveLocked(sess *pluginapi.UISession) bool {
	for _, pl := range s.placements {
		if pl.ID == s.activeID {
			dims := s.pageDimsAtLocked(pl.Page)
			s.nextSigSeq++
			clone := pl
			clone.ID = fmt.Sprintf("sig_%d", s.nextSigSeq)
			clone.X = min(pl.X+16, max(dims.W-pl.Width, 0))
			clone.Y = min(pl.Y+16, max(dims.H-pl.Height, 0))
			s.placements = append(s.placements, clone)
			s.activeID = clone.ID
			s.refreshPlacementAssetLocked(clone, sess)
			return true
		}
	}
	return false
}

func (s *pdfSignServer) applyActiveToAllPagesLocked(sess *pluginapi.UISession) bool {
	var src *pdfsign.PlacedSignature
	for i := range s.placements {
		if s.placements[i].ID == s.activeID {
			src = &s.placements[i]
			break
		}
	}
	if src == nil {
		return false
	}
	for p := 1; p <= s.pageCount; p++ {
		if p == src.Page {
			continue
		}
		s.nextSigSeq++
		clone := *src
		clone.ID = fmt.Sprintf("sig_%d", s.nextSigSeq)
		clone.Page = p
		s.placements = append(s.placements, clone)
		s.refreshPlacementAssetLocked(clone, sess)
	}
	return true
}

func (s *pdfSignServer) deleteActiveLocked() bool {
	if s.activeID == "" {
		return false
	}
	idx := slices.IndexFunc(s.placements, func(pl pdfsign.PlacedSignature) bool {
		return pl.ID == s.activeID
	})
	if idx < 0 {
		return false
	}
	removed := s.placements[idx]
	s.placements = append(s.placements[:idx], s.placements[idx+1:]...)
	s.activeID = ""
	if s.sess != nil {
		if fn, ok := s.placedFiles[removed.ID]; ok && fn != "" {
			_ = os.Remove(filepath.Join(s.sess.AssetsDir(), fn))
			delete(s.placedFiles, removed.ID)
			delete(s.placedAssets, removed.ID)
		}
	}
	return true
}

func (s *pdfSignServer) startExportLocked(outPath string, sess *pluginapi.UISession) {
	if s.pdfPath == "" || len(s.placements) == 0 || s.exporting {
		return
	}
	if s.exportCancel != nil {
		s.exportCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.exportCancel = cancel
	s.exportGen++
	gen := s.exportGen
	s.exporting = true
	s.exportDone = false
	s.exportErr = ""
	s.exportPath = outPath

	inPath := s.pdfPath
	pageCount := s.pageCount
	placementsCopy := make([]pdfsign.PlacedSignature, len(s.placements))
	copy(placementsCopy, s.placements)

	go func() {
		defer cancel()
		err := pdfsign.SignPDF(ctx, pdfsign.SignRequest{
			InFile:     inPath,
			OutFile:    outPath,
			Signatures: placementsCopy,
		}, nil)

		s.mu.Lock()
		if s.exportGen != gen {
			s.mu.Unlock()
			return
		}
		s.exporting = false
		s.exportCancel = nil
		if err != nil {
			s.exportErr = err.Error()
		} else {
			s.exportDone = true
			if s.history != nil {
				_ = s.history.Add(pdfsign.HistoryItem{
					ID:             fmt.Sprintf("sign_%d", time.Now().UnixNano()),
					Summary:        fmt.Sprintf("Signed %s (%d signatures)", filepath.Base(outPath), len(placementsCopy)),
					OutputFile:     outPath,
					SignatureCount: len(placementsCopy),
					PageCount:      pageCount,
					CompletedAt:    time.Now(),
				})
			}
		}
		tree := s.buildTreeLocked()
		s.mu.Unlock()

		if sess != nil {
			_ = sess.Render(tree)
		}
	}()
}

func (s *pdfSignServer) syncVaultAssetsLocked(sess *pluginapi.UISession) {
	if sess == nil || s.vault == nil {
		return
	}
	dir := sess.AssetsDir()
	items := s.vault.List()
	s.vaultAssets = make(map[string]string, len(items))
	for _, it := range items {
		_, pngBytes, err := s.vault.Get(it.ID)
		if err != nil || len(pngBytes) == 0 {
			continue
		}
		name := "vault-" + it.ID + ".png"
		final := filepath.Join(dir, name)
		tmp := filepath.Join(dir, ".tmp-"+name)
		if err := os.WriteFile(tmp, pngBytes, 0o600); err == nil {
			if err := os.Rename(tmp, final); err == nil {
				s.vaultAssets[it.ID] = "asset:" + name
			}
		}
	}
}

func writeAssetPNG(dir, name string, img image.Image) error {
	if dir == "" || img == nil {
		return nil
	}
	data, err := pdfsign.EncodePNGToBytes(img)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".tmp-"+name)
	final := filepath.Join(dir, name)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func studioInkRGBA(idx int) color.RGBA {
	switch idx {
	case 1:
		return color.RGBA{R: 20, G: 50, B: 180, A: 255}
	case 2:
		return color.RGBA{R: 185, G: 28, B: 28, A: 255}
	default:
		return color.RGBA{R: 20, G: 20, B: 20, A: 255}
	}
}

func studioInkToken(idx int) string {
	switch idx {
	case 1:
		return "blue_ink"
	case 2:
		return "danger"
	default:
		return "ink"
	}
}

func parseStrokePoints(raw string) []pdfsign.Point {
	parts := strings.Split(raw, ";")
	pts := make([]pdfsign.Point, 0, len(parts))
	for _, part := range parts {
		xy := strings.SplitN(part, ",", 2)
		if len(xy) != 2 {
			continue
		}
		x, err1 := strconv.ParseFloat(strings.TrimSpace(xy[0]), 64)
		y, err2 := strconv.ParseFloat(strings.TrimSpace(xy[1]), 64)
		if err1 == nil && err2 == nil {
			pts = append(pts, pdfsign.Point{X: x, Y: y})
		}
	}
	return pts
}

func parseDragCoords(raw string) (float64, float64, bool) {
	xy := strings.SplitN(raw, ",", 2)
	if len(xy) != 2 {
		return 0, 0, false
	}
	x, err1 := strconv.ParseFloat(strings.TrimSpace(xy[0]), 64)
	y, err2 := strconv.ParseFloat(strings.TrimSpace(xy[1]), 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return x, y, true
}

func parseResizeRect(raw string) (float64, float64, float64, float64, bool) {
	parts := strings.SplitN(raw, ",", 4)
	if len(parts) != 4 {
		return 0, 0, 0, 0, false
	}
	x, e1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	y, e2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	w, e3 := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
	h, e4 := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || w <= 0 || h <= 0 {
		return 0, 0, 0, 0, false
	}
	return x, y, w, h, true
}

func mustProps(v any) map[string]json.RawMessage {
	raw, _ := json.Marshal(v)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	return m
}

func main() {
	identity := pluginapi.InitializeResult{
		Protocol: pluginapi.Protocol,
		PluginID: "com.mtools.pdfsign",
		Version:  "1.0.0",
	}
	server := newPDFSignServer()
	if err := pluginapi.ServeLive(context.Background(), os.Stdin, os.Stdout, identity, server, server); err != nil {
		os.Exit(1)
	}
}
