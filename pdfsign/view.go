package pdfsign

import (
	"context"
	"fmt"
	"image"
	"math"
	"path/filepath"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"

	"github.com/seiortech/mtools-sdk/job"
	"github.com/seiortech/mtools-sdk/ui/components"
	"github.com/seiortech/mtools-sdk/ui/theme"
)

type placementOpKind int

const (
	opDelete placementOpKind = iota
	opDuplicate
	opApplyAllPages
	opRotate90
	opToggleLock
)

type pendingPlacementOp struct {
	kind  placementOpKind
	sigID string
}

// ViewState holds the active document, placements, and UI interaction states.
type ViewState struct {
	// Active Document
	PDFPath     string
	PDFName     string
	PageCount   int
	CurrentPage int
	PageDims    []types.Dim
	Zoom        float64 // 0.25 to 3.0, default 1.0

	// Multi-page connected scroll state
	Scroll ui.ScrollState

	// Collapsible Right Panel
	RightPanelCollapsed bool
	RightPanelWidth     int // default 320, min 240, max 460

	// Rendering status
	IsRendering bool
	RenderError string

	// Signatures placed on the document
	Placements []PlacedSignature
	SelectedID string

	// Deferred actions queue to prevent index-out-of-range panic during render iterations
	pendingOps []pendingPlacementOp

	// Drag-and-drop from Saved Signatures vault onto PDF page canvas
	RootHandle        ui.Handle
	PageCanvasHandles map[int]*ui.Handle
	PointerX          float32
	PointerY          float32
	DragActive        bool
	DragVaultID       string
	DragImg           image.Image
	DragBmp           *ui.Bitmap
	DragPressX        float32
	DragPressY        float32
	DragPressID       string

	// Studio Modal
	StudioOpen  bool
	StudioState *StudioState

	// Export / Sign Status
	ActiveJobID    job.JobID
	RecordedJobIDs map[job.JobID]bool
	IsExporting    bool
	ExportStatus   string
	ExportError    string
	LastSignedFile string
}

// NewViewState initializes the default view state.
func NewViewState() *ViewState {
	return &ViewState{
		CurrentPage:       1,
		Zoom:              1.0,
		RightPanelWidth:   280,
		PageCanvasHandles: make(map[int]*ui.Handle),
		StudioState:       NewStudioState(),
		RecordedJobIDs:    make(map[job.JobID]bool),
	}
}

func (st *ViewState) getPageCanvasHandle(pageNum int) *ui.Handle {
	if st.PageCanvasHandles == nil {
		st.PageCanvasHandles = make(map[int]*ui.Handle)
	}
	h, ok := st.PageCanvasHandles[pageNum]
	if !ok {
		h = &ui.Handle{}
		st.PageCanvasHandles[pageNum] = h
	}
	return h
}

func (st *ViewState) clearDrag() {
	st.DragActive = false
	st.DragVaultID = ""
	st.DragImg = nil
	st.DragBmp = nil
	st.DragPressID = ""
}

func (st *ViewState) findPlacement(id string) *PlacedSignature {
	for i := range st.Placements {
		if st.Placements[i].ID == id {
			return &st.Placements[i]
		}
	}
	return nil
}

func (st *ViewState) queueOp(kind placementOpKind, sigID string) {
	st.pendingOps = append(st.pendingOps, pendingPlacementOp{kind: kind, sigID: sigID})
}

func (p *Plugin) applyPendingPlacementActions(st *ViewState) {
	if len(st.pendingOps) == 0 {
		return
	}
	ops := st.pendingOps
	st.pendingOps = nil

	for _, op := range ops {
		switch op.kind {
		case opDelete:
			p.deletePlacedSignature(op.sigID)
		case opToggleLock:
			if sig := st.findPlacement(op.sigID); sig != nil {
				sig.Locked = !sig.Locked
			}
		case opDuplicate:
			if sig := st.findPlacement(op.sigID); sig != nil {
				newSig := *sig
				newSig.ID = fmt.Sprintf("sig-%d", time.Now().UnixNano())
				newSig.X += 20
				newSig.Y += 20
				newSig.cachedBmp = nil
				st.Placements = append(st.Placements, newSig)
				st.SelectedID = newSig.ID
			}
		case opApplyAllPages:
			if sig := st.findPlacement(op.sigID); sig != nil {
				orig := *sig
				for pg := 1; pg <= st.PageCount; pg++ {
					if pg == orig.Page {
						continue
					}
					newSig := orig
					newSig.ID = fmt.Sprintf("sig-pg%d-%d", pg, time.Now().UnixNano())
					newSig.Page = pg
					newSig.cachedBmp = nil
					st.Placements = append(st.Placements, newSig)
				}
			}
		case opRotate90:
			if sig := st.findPlacement(op.sigID); sig != nil {
				sig.Rotation += 90
				if sig.Rotation >= 360 {
					sig.Rotation = 0
				}
				// Swap Width/Height around center so the selection box matches the rotated image.
				cx := sig.X + sig.Width/2
				cy := sig.Y + sig.Height/2
				sig.Width, sig.Height = sig.Height, sig.Width
				sig.X = cx - sig.Width/2
				sig.Y = cy - sig.Height/2
				if sig.Page >= 1 && sig.Page <= len(st.PageDims) {
					dim := st.PageDims[sig.Page-1]
					if sig.X < 0 {
						sig.X = 0
					} else if sig.X+sig.Width > dim.Width {
						sig.X = mathMax(0, dim.Width-sig.Width)
					}
					if sig.Y < 0 {
						sig.Y = 0
					} else if sig.Y+sig.Height > dim.Height {
						sig.Y = mathMax(0, dim.Height-sig.Height)
					}
				}
				sig.cachedBmp = nil
			}
		}
	}
}

// syncActiveJob polls active job progress and completion, recording history on success.
func (p *Plugin) syncActiveJob(st *ViewState) {
	if p.queue == nil || st.ActiveJobID == "" {
		return
	}
	rec, ok := p.queue.GetRecord(st.ActiveJobID)
	if !ok {
		return
	}
	switch rec.Status {
	case job.StatusRunning, job.StatusQueued:
		st.IsExporting = true
		if rec.Progress.Message != "" {
			st.ExportStatus = rec.Progress.Message
		} else {
			st.ExportStatus = "Signing document..."
		}
	case job.StatusSucceeded:
		st.IsExporting = false
		st.ExportStatus = rec.Result.Summary
		st.ExportError = ""
		if len(rec.Result.Outputs) > 0 {
			st.LastSignedFile = rec.Result.Outputs[0]
		}
		if !st.RecordedJobIDs[rec.ID] {
			st.RecordedJobIDs[rec.ID] = true
			p.History().Add(HistoryItem{
				ID:             string(rec.ID),
				Summary:        rec.Result.Summary,
				OutputFile:     st.LastSignedFile,
				SignatureCount: len(st.Placements),
				PageCount:      st.PageCount,
				CompletedAt:    time.Now(),
			})
		}
	case job.StatusFailed:
		st.IsExporting = false
		if rec.Error != "" {
			st.ExportError = rec.Error
		} else {
			st.ExportError = "Signing failed"
		}
	case job.StatusCanceled:
		st.IsExporting = false
		st.ExportStatus = "Canceled"
	}
}

// RenderWorkspace renders the complete PDF Signature editor surface.
func (p *Plugin) RenderWorkspace(c *ui.Context) {
	st := p.state
	if st == nil {
		p.state = NewViewState()
		st = p.state
	}

	// Poll background export job and record history on success
	p.syncActiveJob(st)

	// Apply any deferred placement mutations before building the tree
	p.applyPendingPlacementActions(st)

	// Global canvas shortcuts: Escape to deselect or cancel drag, L or Cmd+L to lock/unlock active signature
	if c.Shortcut(0, ui.KeyEscape) {
		if st.DragActive {
			st.clearDrag()
			c.Invalidate()
		} else if st.SelectedID != "" {
			st.SelectedID = ""
			c.Invalidate()
		}
	}
	if st.SelectedID != "" && (c.Shortcut(0, ui.KeyL) || c.Shortcut(ui.Cmd, ui.KeyL) || c.Shortcut(ui.Ctrl, ui.KeyL)) {
		st.queueOp(opToggleLock, st.SelectedID)
		c.Invalidate()
	}

	rootBounds := st.RootHandle.Bounds(c)
	ws := ui.Column(c.Key("pdfsign-workspace")).Bind(&st.RootHandle).Fill().Gap(0).Background(theme.Ground)
	ws.ObserveInput(func(ev ui.InputEvent) {
		switch ev.Kind {
		case ui.InputPointerDown, ui.InputPointerMove, ui.InputPointerUp:
			rb := st.RootHandle.Bounds(c)
			st.PointerX = rb.X + ev.X
			st.PointerY = rb.Y + ev.Y
			if st.DragActive && ev.Kind == ui.InputPointerUp {
				p.completeVaultDrop(c, st)
			} else if ev.Kind == ui.InputPointerUp {
				st.DragPressID = ""
			}
		}
	})

	ws.Children(func() {
		if st.PDFPath == "" {
			p.renderEmptyState(c, st)
		} else {
			// Top Workspace Bar: Open, filename, page stepper, zoom, Finish & Sign, rail toggle
			p.renderTopBar(c, st)

			// Main Editor Body: Viewport + Resize Handle + Signatures Rail
			ui.Row(c.Key("pdfsign-body")).Fill().Gap(0).Background(theme.Ground).Children(func() {
				// Left side: Continuous connected multi-page PDF reader
				p.renderViewport(c, st)

				// Center: Splitter resize handle between canvas and right rail
				if !st.RightPanelCollapsed {
					components.VerticalResizeHandle(c, components.ResizeHandleProps{
						Width:        st.RightPanelWidth,
						MinWidth:     240,
						MaxWidth:     420,
						DefaultWidth: 280,
						OnResize: func(newWidth int) {
							st.RightPanelWidth = newWidth
							c.Invalidate()
						},
						OnReset: func() {
							st.RightPanelWidth = 280
							c.Invalidate()
						},
					})
				}

				// Right side: Collapsible Signatures rail & contextual inspector
				p.renderRightPanel(c, st)
			})
		}

		// Floating drag-and-drop ghost preview
		if st.DragActive && st.DragBmp != nil && st.DragImg != nil {
			sigW, sigH := defaultPlacementSize(st.DragImg)
			ghostW := float32(sigW * st.Zoom)
			ghostH := float32(sigH * st.Zoom)
			if ghostW < 60 {
				ghostW = 60
			}
			if ghostH < 30 {
				ghostH = 30
			}
			ghostX := st.PointerX - rootBounds.X - ghostW/2
			ghostY := st.PointerY - rootBounds.Y - ghostH/2
			ui.Box(c.Key("vault-drag-ghost")).
				Absolute().Left(ghostX).Top(ghostY).Width(ghostW).Height(ghostH).
				Border(2, theme.Accent).Background(theme.Paper).Radius(0).
				Justify(ui.Center).AlignItems(ui.Center).Children(func() {
				ui.Image(c, st.DragBmp).Width(ghostW - 4).Height(ghostH - 4)
			})
		}

		// Signature Studio Modal (Draw / Photo background removal)
		if st.StudioOpen {
			p.renderStudioDialog(c)
		}
	})
}

// renderEmptyState renders the initial prompt when no PDF is opened.
func (p *Plugin) renderEmptyState(c *ui.Context, st *ViewState) {
	ui.Column(c.Key("pdfsign-empty-state")).
		Fill().Justify(ui.Center).AlignItems(ui.Center).Gap(16).Children(func() {

		ui.Box(c).Width(440).Padding(32).Gap(16).
			Background(theme.Paper).Border(4, theme.Ink).Radius(0).
			Shadow(6, 6, 0, 0, theme.Ink).AlignItems(ui.Center).Children(func() {

			ui.Icon(c, components.IconSignature).Size(36, 36).TextColor(theme.Accent)
			ui.Text(c, p.t("pdfsign.empty_title")).FontSize(20).FontWeight(700).Role(ui.RoleHeading).Level(2)
			ui.Text(c, p.t("pdfsign.empty_desc")).FontSize(14).TextColor(theme.InkMuted).TextAlign(ui.Center)

			components.PrimaryButton(c.Key("empty-open-btn"), p.t("pdfsign.btn_select_file"), func() {
				go func() {
					paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
						Filters: []mygo.FileFilter{{Name: "PDF Documents", Extensions: []string{"pdf"}}},
					})
					if err == nil && len(paths) > 0 {
						p.LoadPDF(paths[0])
						p.requestRedraw()
					}
				}()
			}).Height(44).FocusRingColor(theme.Paper)
		})
	})
}

// pageTopY computes the cumulative Y offset of a given page in the continuous scroll view.
func (p *Plugin) pageTopY(st *ViewState, pageNum int) float32 {
	const pageGap = float32(24)
	const topPadding = float32(24)
	y := topPadding
	for i := 1; i < pageNum && i <= len(st.PageDims); i++ {
		dim := st.PageDims[i-1]
		pageH := float32(dim.Height * st.Zoom)
		y += pageH + pageGap
	}
	return y
}

func (p *Plugin) scrollToPage(st *ViewState, pageNum int) {
	if pageNum < 1 {
		pageNum = 1
	}
	if pageNum > st.PageCount {
		pageNum = st.PageCount
	}
	st.CurrentPage = pageNum
	targetY := p.pageTopY(st, pageNum) - 24
	if targetY < 0 {
		targetY = 0
	}
	st.Scroll.ScrollTo(0, targetY, ui.Smooth)
	p.requestPageRender(pageNum)
}

// syncCurrentPageFromScroll updates st.CurrentPage based on the current scroll position.
func (p *Plugin) syncCurrentPageFromScroll(st *ViewState) {
	if st.PageCount <= 1 || len(st.PageDims) == 0 || st.Scroll.Scrolling() {
		return
	}
	// Focus point is 200 DIP down the scroll viewport
	focusY := st.Scroll.Y + 200
	if focusY < 0 {
		focusY = 0
	}

	const pageGap = float32(24)
	const topPadding = float32(24)
	currY := topPadding
	foundPage := 1

	for i := 1; i <= st.PageCount && i <= len(st.PageDims); i++ {
		dim := st.PageDims[i-1]
		pageH := float32(dim.Height * st.Zoom)
		bottomY := currY + pageH
		if focusY >= currY && focusY <= bottomY+pageGap {
			foundPage = i
			break
		}
		if focusY < currY {
			break
		}
		foundPage = i
		currY = bottomY + pageGap
	}

	if foundPage != st.CurrentPage {
		st.CurrentPage = foundPage
		p.requestPageRender(foundPage)
	}
}

// iconSquareBtn renders a clean Bauhaus square icon button with label, tooltip, and disabled state.
func iconSquareBtn(c *ui.Context, key string, icon *ui.SVG, label string, size float32, disabled bool, onClick func()) ui.Element {
	b := ui.ButtonBase(c.Key(key)).
		Width(size).Height(size).Radius(0).
		Background(theme.Paper).Border(1, theme.BorderQuiet).
		Justify(ui.Center).AlignItems(ui.Center).
		Label(label).Tooltip(label).Disabled(disabled)

	if !disabled {
		b.Cursor(ui.CursorPointer)
		if b.Pressed() {
			b.Background(theme.SurfacePressed)
		} else if b.Hovered() {
			b.Background(theme.SurfaceHover).Border(1, theme.Ink)
		}
		if b.Clicked() && onClick != nil {
			onClick()
		}
	} else {
		b.Background(theme.Ground)
	}

	icColor := theme.Ink
	if disabled {
		icColor = theme.InkMuted
	}
	b.Children(func() {
		ui.Icon(c, icon).Size(15, 15).TextColor(icColor)
	})
	return b
}

func (p *Plugin) fitZoomToViewport(c *ui.Context, st *ViewState) {
	bounds := st.Scroll.Bounds(c)
	availW := float32(bounds.W) - 48
	availH := float32(bounds.H) - 48
	if availW <= 0 || availH <= 0 {
		winW, winH := c.Size()
		rightPanelW := float32(st.RightPanelWidth)
		if st.RightPanelCollapsed {
			rightPanelW = 48
		}
		availW = float32(winW) - rightPanelW - 48
		availH = float32(winH) - 48 - 48
	}
	pageW := float32(595)
	pageH := float32(842)
	if len(st.PageDims) > 0 && st.PageDims[0].Width > 0 {
		pageW = float32(st.PageDims[0].Width)
		pageH = float32(st.PageDims[0].Height)
	}
	fitW := float64((availW - 16) / pageW)
	fitH := float64((availH - 16) / pageH)
	fit := math.Min(fitW, fitH)
	st.Zoom = math.Max(0.25, math.Min(2.0, fit))
	st.Scroll.X = 0
	p.requestPageRender(st.CurrentPage)
	c.Invalidate()
}

// renderTopBar renders the unified 48 DIP workspace bar with Open, filename, page stepper, zoom, and Finish & Sign.
func (p *Plugin) renderTopBar(c *ui.Context, st *ViewState) {
	ui.Row(c.Key("pdfsign-topbar")).
		FillWidth().Height(48).Padding(0, 12).Gap(12).
		Background(theme.Paper).BorderWidth(0, 0, 4, 0).BorderColor(theme.Ink).
		Justify(ui.SpaceBetween).AlignItems(ui.Center).Children(func() {

		// Left: Open PDF + Document Name
		ui.Row(c).Gap(10).AlignItems(ui.Center).Grow(1).Children(func() {
			components.SecondaryButton(c.Key("topbar-open-btn"), "Open", func() {
				go func() {
					paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
						Filters: []mygo.FileFilter{{Name: "PDF Documents", Extensions: []string{"pdf"}}},
					})
					if err == nil && len(paths) > 0 {
						p.LoadPDF(paths[0])
						p.requestRedraw()
					}
				}()
			}).Height(32).Padding(0, 12).Label("Open PDF").Tooltip("Open another PDF document")

			if st.PDFName != "" {
				ui.Text(c, st.PDFName).FontSize(14).FontWeight(600).SingleLine().Tooltip(st.PDFName)
			}
		})

		// Center: Page Stepper + Zoom Cluster
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			// Page stepper
			ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
				iconSquareBtn(c, "topbar-prev-page", components.IconChevronLeft, "Previous page", 32, st.CurrentPage <= 1, func() {
					p.scrollToPage(st, st.CurrentPage-1)
					c.Invalidate()
				})
				ui.Text(c, fmt.Sprintf("%d / %d", st.CurrentPage, st.PageCount)).
					FontSize(13).FontWeight(600).FontFeatures("tnum").SingleLine().Padding(0, 6)
				iconSquareBtn(c, "topbar-next-page", components.IconChevronRight, "Next page", 32, st.CurrentPage >= st.PageCount, func() {
					p.scrollToPage(st, st.CurrentPage+1)
					c.Invalidate()
				})
			})

			// Inner vertical divider
			ui.Box(c).Width(1).Height(20).Background(theme.BorderQuiet)

			// Zoom cluster
			ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
				iconSquareBtn(c, "topbar-zoom-out", components.IconZoomOut, "Zoom out", 32, st.Zoom <= 0.25, func() {
					st.Zoom = math.Max(0.25, st.Zoom-0.15)
					p.requestPageRender(st.CurrentPage)
					c.Invalidate()
				})
				components.SecondaryButton(c.Key("topbar-zoom-reset"), fmt.Sprintf("%.0f%%", st.Zoom*100), func() {
					st.Zoom = 1.0
					st.Scroll.X = 0
					p.requestPageRender(st.CurrentPage)
					c.Invalidate()
				}).Height(32).Padding(0, 10).FontFeatures("tnum").Tooltip("Reset zoom to 100%")
				iconSquareBtn(c, "topbar-zoom-in", components.IconZoomIn, "Zoom in", 32, st.Zoom >= 3.0, func() {
					st.Zoom = math.Min(3.0, st.Zoom+0.15)
					p.requestPageRender(st.CurrentPage)
					c.Invalidate()
				})
				components.SecondaryButton(c.Key("topbar-zoom-fit"), p.t("pdfsign.zoom_fit"), func() {
					p.fitZoomToViewport(c, st)
				}).Height(32).Padding(0, 10)
			})
		})

		// Right: Primary Finish & Sign + Collapse Rail Toggle
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			components.PrimaryButton(c.Key("panel-sign-export-btn"), p.t("pdfsign.btn_finish_export"), func() {
				p.promptExport(c)
			}).Height(34).Padding(0, 16).FocusRingColor(theme.Paper).Disabled(len(st.Placements) == 0 || st.IsExporting)

			railIcon := components.IconCollapseRight
			railLabel := "Collapse signatures panel"
			if st.RightPanelCollapsed {
				railIcon = components.IconCollapseLeft
				railLabel = "Expand signatures panel"
			}
			iconSquareBtn(c, "topbar-toggle-rail", railIcon, railLabel, 32, false, func() {
				st.RightPanelCollapsed = !st.RightPanelCollapsed
				c.Invalidate()
			})
		})
	})
}

// renderViewport renders all connected PDF pages vertically inside a smooth continuous scroll view.
func (p *Plugin) renderViewport(c *ui.Context, st *ViewState) {
	p.syncCurrentPageFromScroll(st)

	// Determine available dimensions for exact centering on Fit or zoom < 100%
	bounds := st.Scroll.Bounds(c)
	var availW, availH float32
	if bounds.W > 0 && bounds.H > 0 {
		availW = float32(bounds.W) - 48
		availH = float32(bounds.H) - 48
	} else {
		winW, winH := c.Size()
		rightPanelW := float32(st.RightPanelWidth)
		if st.RightPanelCollapsed {
			rightPanelW = 48
		}
		availW = float32(winW) - rightPanelW - 48
		availH = float32(winH) - 48 - 48
	}
	if availW < 0 {
		availW = 0
	}
	if availH < 0 {
		availH = 0
	}

	viewport := ui.ScrollBoth(c.Key("pdfsign-viewport")).
		TrackScroll(&st.Scroll).
		Grow(1).FillHeight().Background(theme.Ground).Padding(24)

	// Gesture & Input Handling: trackpad pinch-to-zoom (InputZoom) & Cmd/Ctrl+Scroll
	viewport.HandleInput(func(ev ui.InputEvent) bool {
		if ev.Kind == ui.InputZoom && ev.Scale > 0 {
			newZoom := math.Max(0.25, math.Min(3.0, st.Zoom*float64(ev.Scale)))
			if math.Abs(newZoom-st.Zoom) > 0.005 {
				st.Zoom = newZoom
				p.bumpZoomGen()
				p.scheduleDebouncedZoomRender(100 * time.Millisecond)
				c.Invalidate()
			}
			return true
		}
		if ev.Kind == ui.InputScroll && (ev.Mods&ui.Cmd != 0 || ev.Mods&ui.Ctrl != 0) {
			factor := 1.0 - float64(ev.DY)*0.01
			newZoom := math.Max(0.25, math.Min(3.0, st.Zoom*factor))
			if math.Abs(newZoom-st.Zoom) > 0.005 {
				st.Zoom = newZoom
				p.bumpZoomGen()
				p.scheduleDebouncedZoomRender(100 * time.Millisecond)
				c.Invalidate()
			}
			return true
		}
		return false
	})

	// Clicking viewport empty margin deselects active signature
	if viewport.Clicked() {
		if st.SelectedID != "" {
			st.SelectedID = ""
			c.Invalidate()
		}
	}

	viewport.Children(func() {
		ui.Column(c.Key("pdfsign-pages-stack")).
			MinWidth(availW).MinHeight(availH).
			AlignItems(ui.Center).Justify(ui.Center).Gap(24).Children(func() {
			const pageGap = float32(24)
			const preloadMargin = float32(400)
			currY := float32(24)
			for pageNum := 1; pageNum <= st.PageCount; pageNum++ {
				pageDim := types.Dim{Width: 595, Height: 842}
				if pageNum-1 >= 0 && pageNum-1 < len(st.PageDims) {
					pageDim = st.PageDims[pageNum-1]
				}
				viewW := float32(pageDim.Width * st.Zoom)
				viewH := float32(pageDim.Height * st.Zoom)
				pageTop := currY
				pageBottom := currY + viewH
				currY = pageBottom + pageGap

				if availH > 0 && (pageBottom < st.Scroll.Y-preloadMargin || pageTop > st.Scroll.Y+availH+preloadMargin) {
					ui.Box(c.Key(fmt.Sprintf("page-spacer-%d", pageNum))).Width(viewW).Height(viewH)
					continue
				}
				p.renderPageInScrollStack(c, st, pageNum)
			}
		})
	})
}

// renderPageInScrollStack renders an individual page and its overlays in the vertical continuous stack.
func (p *Plugin) renderPageInScrollStack(c *ui.Context, st *ViewState, pageNum int) {
	pageDim := types.Dim{Width: 595, Height: 842}
	if pageNum-1 >= 0 && pageNum-1 < len(st.PageDims) {
		pageDim = st.PageDims[pageNum-1]
	}
	viewW := float32(pageDim.Width * st.Zoom)
	viewH := float32(pageDim.Height * st.Zoom)

	renderScale := QuantizeRenderScale(st.Zoom)
	pageBmp, ok := p.pageCache.GetBitmap(st.PDFPath, pageNum, renderScale)
	if !ok {
		// Asynchronously request rendering if near current page
		if abs(pageNum-st.CurrentPage) <= 2 {
			p.requestPageRender(pageNum)
		}
		// Zero-flicker pinch zooming: fall back to closest cached bitmap while HiDPI renders
		if nearestBmp, hasNearest := p.pageCache.GetNearestBitmap(st.PDFPath, pageNum, renderScale); hasNearest {
			pageBmp = nearestBmp
			ok = true
		}
	}

	if !ok {
		ui.Box(c.Key(fmt.Sprintf("page-placeholder-%d", pageNum))).
			Width(viewW).Height(viewH).
			Background(theme.Paper).Border(2, theme.Ink).Radius(0).
			Shadow(4, 4, 0, 0, theme.Ink).
			Justify(ui.Center).AlignItems(ui.Center).Children(func() {
			ui.Text(c, fmt.Sprintf("%s (%d)", p.t("pdfsign.rendering_page"), pageNum)).
				FontSize(14).TextColor(theme.InkMuted)
		})
		return
	}

	pageHandle := st.getPageCanvasHandle(pageNum)
	borderColor := theme.Ink
	borderW := float32(2)
	if st.DragActive {
		pb := pageHandle.Bounds(c)
		if pb.W > 0 && pb.H > 0 &&
			st.PointerX >= pb.X && st.PointerX <= pb.X+pb.W &&
			st.PointerY >= pb.Y && st.PointerY <= pb.Y+pb.H {
			borderColor = theme.Accent
			borderW = 3
		}
	}

	canvas := ui.Box(c.Key(fmt.Sprintf("page-canvas-%d", pageNum))).
		Bind(pageHandle).
		Width(viewW).Height(viewH).
		Background(theme.Paper).Border(borderW, borderColor).Radius(0).
		Shadow(6, 6, 0, 0, theme.Ink)

	// Clicking canvas background deselects active signature
	if canvas.Clicked() {
		if st.SelectedID != "" {
			st.SelectedID = ""
			c.Invalidate()
		}
	}

	canvas.Children(func() {
		// Base rendered PDF page image (cached *ui.Bitmap reused across frames)
		ui.Image(c, pageBmp).Width(viewW).Height(viewH)

		// Page number badge at bottom-right corner of each page
		ui.Box(c.Key(fmt.Sprintf("page-tag-%d", pageNum))).
			Absolute().Right(8).Bottom(8).
			Padding(2, 6).Background(theme.Paper).Border(1, theme.BorderQuiet).Children(func() {
			ui.Text(c, fmt.Sprintf("Page %d", pageNum)).FontSize(11).FontWeight(600).FontFeatures("tnum").TextColor(theme.InkMuted)
		})

		// Render all signatures placed on this specific page
		for i := range st.Placements {
			if st.Placements[i].Page == pageNum {
				p.renderPlacedSignature(c, st, &st.Placements[i], viewW, viewH)
			}
		}
	})
}

// renderPlacedSignature renders a placed signature with absolute positioning, Dragged() tracking, and controls.
func (p *Plugin) renderPlacedSignature(c *ui.Context, st *ViewState, sig *PlacedSignature, pageW, pageH float32) {
	zoom := float32(st.Zoom)
	sigX := float32(sig.X) * zoom
	sigY := float32(sig.Y) * zoom
	sigW := float32(sig.Width) * zoom
	sigH := float32(sig.Height) * zoom

	isSelected := sig.ID == st.SelectedID

	sigBox := ui.Box(c.Key("placed-sig-"+sig.ID)).
		Absolute().Left(sigX).Top(sigY).Width(sigW).Height(sigH)

	if sig.Locked {
		sigBox.Cursor(ui.CursorDefault)
	} else {
		sigBox.Cursor(ui.CursorMove)
	}

	if isSelected {
		borderCol := theme.Accent
		if sig.Locked {
			borderCol = theme.InkMuted
		}
		sigBox.Border(2, borderCol)
	}

	if sigBox.Clicked() {
		st.SelectedID = sig.ID
		st.CurrentPage = sig.Page
		c.Invalidate()
	}

	// Dragging via MyGo Dragged() delta API (disabled when locked)
	if !sig.Locked {
		dx, dy, held := sigBox.Dragged()
		if held && (dx != 0 || dy != 0) {
			st.SelectedID = sig.ID
			st.CurrentPage = sig.Page
			sig.X += float64(dx / zoom)
			sig.Y += float64(dy / zoom)

			// Clamp inside page bounds
			maxX := float64(pageW/zoom) - sig.Width
			maxY := float64(pageH/zoom) - sig.Height
			if sig.X < 0 {
				sig.X = 0
			} else if sig.X > maxX {
				sig.X = maxX
			}
			if sig.Y < 0 {
				sig.Y = 0
			} else if sig.Y > maxY {
				sig.Y = maxY
			}
			c.Invalidate()
		}
	}

	sigBox.Children(func() {
		// Render signature raster image (using cached ui.Bitmap if valid)
		if sig.Image != nil {
			bmp := p.getOrCreatePlacementBitmap(sig)
			if bmp != nil {
				ui.Image(c, bmp).Width(sigW).Height(sigH)
			}
		}

		// Lock indicator badge when locked
		if sig.Locked {
			ui.Box(c.Key("lock-badge-"+sig.ID)).
				Absolute().Left(4).Top(4).Padding(2).
				Background(theme.Paper).Border(1, theme.Ink).Radius(0).Children(func() {
				ui.Icon(c, components.IconLock).Size(11, 11).TextColor(theme.Ink)
			})
		}

		// 8 corner/edge resize handles when selected (unlocked only)
		if isSelected && !sig.Locked {
			p.renderResizeHandles(c, st, sig, sigW, sigH)
		}
	})

	// Floating action toolbar when selected
	if isSelected {
		p.renderFloatingToolbar(c, st, sig, sigX, sigY, sigW, sigH, pageW)
	}
}

func (p *Plugin) getOrCreatePlacementBitmap(sig *PlacedSignature) *ui.Bitmap {
	crop := sig.Crop
	if crop.W <= 0 || crop.H <= 0 {
		crop = DefaultNormalizedRect()
	}
	opacity := sig.Opacity
	if opacity <= 0 || opacity > 1.0 {
		opacity = 1.0
	}

	if sig.cachedBmp != nil &&
		sig.cachedRot == sig.Rotation &&
		sig.cachedOp == opacity &&
		sig.cachedInk == sig.InkMode &&
		sig.cachedCrop == crop {
		if bmp, ok := sig.cachedBmp.(*ui.Bitmap); ok {
			return bmp
		}
	}

	procImg := sig.Image
	if sig.InkMode != InkOriginal {
		procImg = RecolorSignature(procImg, sig.InkMode)
	}

	comp := RenderPlacementImage(procImg, PlacementRenderOptions{
		Crop:        crop,
		RotationDeg: sig.Rotation,
		Opacity:     opacity,
	})

	bmp := ui.NewBitmap(comp)
	sig.cachedImg = comp
	sig.cachedBmp = bmp
	sig.cachedRot = sig.Rotation
	sig.cachedOp = opacity
	sig.cachedInk = sig.InkMode
	sig.cachedCrop = crop
	return bmp
}

// renderResizeHandles renders 8 interactive grab handles around the selected signature.
func (p *Plugin) renderResizeHandles(c *ui.Context, st *ViewState, sig *PlacedSignature, w, h float32) {
	const handleSize = float32(8)
	zoom := float32(st.Zoom)

	corners := []struct {
		mode   string
		cursor ui.Cursor
		left   float32
		top    float32
	}{
		{"nw", ui.CursorResizeNWSE, 0, 0},
		{"ne", ui.CursorResizeNESW, w - handleSize, 0},
		{"se", ui.CursorResizeNWSE, w - handleSize, h - handleSize},
		{"sw", ui.CursorResizeNESW, 0, h - handleSize},
		{"n", ui.CursorResizeNS, w/2 - handleSize/2, 0},
		{"e", ui.CursorResizeEW, w - handleSize, h/2 - handleSize/2},
		{"s", ui.CursorResizeNS, w/2 - handleSize/2, h - handleSize},
		{"w", ui.CursorResizeEW, 0, h/2 - handleSize/2},
	}

	for _, corner := range corners {
		mode := corner.mode
		hBox := ui.Box(c.Key("handle-"+sig.ID+"-"+mode)).
			Absolute().Left(corner.left).Top(corner.top).
			Width(handleSize).Height(handleSize).
			Background(theme.Paper).Border(1, theme.Accent).
			Cursor(corner.cursor)

		hdx, hdy, hHeld := hBox.Dragged()
		if hHeld && (hdx != 0 || hdy != 0) {
			dx := float64(hdx / zoom)
			dy := float64(hdy / zoom)

			switch mode {
			case "se":
				sig.Width = mathMax(24, sig.Width+dx)
				sig.Height = mathMax(12, sig.Height+dy)
			case "e":
				sig.Width = mathMax(24, sig.Width+dx)
			case "s":
				sig.Height = mathMax(12, sig.Height+dy)
			case "nw":
				nwW := mathMax(24, sig.Width-dx)
				nwH := mathMax(12, sig.Height-dy)
				sig.X += sig.Width - nwW
				sig.Y += sig.Height - nwH
				sig.Width = nwW
				sig.Height = nwH
			case "ne":
				neW := mathMax(24, sig.Width+dx)
				neH := mathMax(12, sig.Height-dy)
				sig.Y += sig.Height - neH
				sig.Width = neW
				sig.Height = neH
			case "sw":
				swW := mathMax(24, sig.Width-dx)
				swH := mathMax(12, sig.Height+dy)
				sig.X += sig.Width - swW
				sig.Width = swW
				sig.Height = swH
			case "n":
				nH := mathMax(12, sig.Height-dy)
				sig.Y += sig.Height - nH
				sig.Height = nH
			case "w":
				wW := mathMax(24, sig.Width-dx)
				sig.X += sig.Width - wW
				sig.Width = wW
			}
			c.Invalidate()
		}
	}
}

// renderFloatingToolbar renders a clean, compact icon pill above the active signature on canvas.
func (p *Plugin) renderFloatingToolbar(c *ui.Context, st *ViewState, sig *PlacedSignature, left, top, w, h, pageW float32) {
	btnCount := 4
	if sig.Locked {
		btnCount = 2 // Lock toggle + Duplicate
	}
	barW := float32(btnCount*24 + (btnCount-1)*2 + 8)
	barLeft := left + (w-barW)/2
	if barLeft < 4 {
		barLeft = 4
	}
	if barLeft+barW > pageW-4 {
		barLeft = pageW - barW - 4
	}
	barTop := top - 32
	if barTop < 4 {
		barTop = top + h + 6
	}

	ui.Row(c.Key("sig-floating-toolbar-"+sig.ID)).
		Absolute().Top(barTop).Left(barLeft).Height(28).Padding(2, 4).Gap(2).
		Background(theme.Paper).Border(1.5, theme.Ink).Radius(0).
		Shadow(2, 2, 0, 0, theme.Ink).AlignItems(ui.Center).Children(func() {

		// 1. Lock / Unlock toggle
		lockBtn := ui.ButtonBase(c.Key("sig-tb-lock-"+sig.ID)).
			Width(24).Height(24).Radius(0).Justify(ui.Center).AlignItems(ui.Center)
		if sig.Locked {
			lockBtn.Background(theme.Accent)
			if lockBtn.Hovered() {
				lockBtn.Background(theme.AccentHover)
			}
		} else if lockBtn.Hovered() {
			lockBtn.Background(theme.SurfaceHover)
		}
		if lockBtn.Clicked() {
			st.queueOp(opToggleLock, sig.ID)
			c.Invalidate()
		}
		lockBtn.Children(func() {
			ic := components.IconUnlock
			icColor := theme.Ink
			if sig.Locked {
				ic = components.IconLock
				icColor = theme.OnField
			}
			ui.Icon(c, ic).Size(14, 14).TextColor(icColor)
		})

		// 2. Rotate +90° (hidden when locked)
		if !sig.Locked {
			rotBtn := ui.ButtonBase(c.Key("sig-tb-rot-"+sig.ID)).
				Width(24).Height(24).Radius(0).Justify(ui.Center).AlignItems(ui.Center)
			if rotBtn.Hovered() {
				rotBtn.Background(theme.SurfaceHover)
			}
			if rotBtn.Clicked() {
				st.queueOp(opRotate90, sig.ID)
				c.Invalidate()
			}
			rotBtn.Children(func() {
				ui.Icon(c, components.IconRotate).Size(14, 14).TextColor(theme.Ink)
			})
		}

		// 3. Duplicate
		dupBtn := ui.ButtonBase(c.Key("sig-tb-dup-"+sig.ID)).
			Width(24).Height(24).Radius(0).Justify(ui.Center).AlignItems(ui.Center)
		if dupBtn.Hovered() {
			dupBtn.Background(theme.SurfaceHover)
		}
		if dupBtn.Clicked() {
			st.queueOp(opDuplicate, sig.ID)
			c.Invalidate()
		}
		dupBtn.Children(func() {
			ui.Icon(c, components.IconCopy).Size(14, 14).TextColor(theme.Ink)
		})

		// 4. Delete (hidden when locked)
		if !sig.Locked {
			delBtn := ui.ButtonBase(c.Key("sig-tb-del-"+sig.ID)).
				Width(24).Height(24).Radius(0).Justify(ui.Center).AlignItems(ui.Center)
			if delBtn.Hovered() {
				delBtn.Background(theme.SurfaceHover)
			}
			if delBtn.Clicked() {
				st.queueOp(opDelete, sig.ID)
				c.Invalidate()
			}
			delBtn.Children(func() {
				ui.Icon(c, components.IconTrash).Size(14, 14).TextColor(theme.Danger)
			})
		}
	})
}

// renderRightPanel renders the collapsible Signatures rail with vault tiles, contextual selection strip, and export status.
func (p *Plugin) renderRightPanel(c *ui.Context, st *ViewState) {
	if st.RightPanelCollapsed {
		// Minimized Rail: 48 DIP width
		ui.Column(c.Key("pdfsign-right-panel-min")).
			Width(48).FillHeight().
			Background(theme.Paper).BorderWidth(0, 0, 0, 4).BorderColor(theme.Ink).
			AlignItems(ui.Center).Padding(12, 6).Gap(12).Children(func() {

			iconSquareBtn(c, "rail-expand-btn", components.IconCollapseLeft, "Expand signatures panel", 32, false, func() {
				st.RightPanelCollapsed = false
				c.Invalidate()
			})

			// Page indicator badge
			ui.Box(c).Padding(4, 6).Background(theme.Ground).Border(1, theme.BorderQuiet).Children(func() {
				ui.Text(c, fmt.Sprintf("P.%d", st.CurrentPage)).FontSize(11).FontWeight(600).FontFeatures("tnum")
			})

			// Placed signatures count
			if len(st.Placements) > 0 {
				ui.Box(c).Padding(4, 6).Background(theme.Accent).Children(func() {
					ui.Text(c, fmt.Sprintf("%d", len(st.Placements))).FontSize(11).FontWeight(700).FontFeatures("tnum").TextColor(theme.OnField)
				})
			}
		})
		return
	}

	panelW := float32(st.RightPanelWidth)
	if panelW < 240 {
		panelW = 280
	}

	ui.Column(c.Key("pdfsign-right-panel")).
		Width(panelW).FillHeight().
		Background(theme.Paper).BorderWidth(0, 0, 0, 4).BorderColor(theme.Ink).Children(func() {

		// Panel Header: Signatures + New
		ui.Row(c.Key("right-panel-header")).
			FillWidth().Height(48).Padding(0, 12).
			BorderWidth(0, 0, 2, 0).BorderColor(theme.BorderQuiet).
			Justify(ui.SpaceBetween).AlignItems(ui.Center).Children(func() {

			ui.Text(c, "Signatures").FontSize(14).FontWeight(700).Role(ui.RoleHeading).Level(3)

			if p.Vault().Count() < MaxSavedSignatures {
				components.PrimaryButton(c.Key("vault-new-btn"), "+ New", func() {
					st.StudioState = NewStudioState()
					st.StudioOpen = true
					c.Invalidate()
				}).Height(30).Padding(0, 12).FocusRingColor(theme.Paper)
			}
		})

		// Scrollable Body
		ui.Scroll(c.Key("right-panel-scroll")).Fill().Padding(12).Children(func() {
			ui.Column(c).FillWidth().Gap(12).Children(func() {
				// 1. Saved Signatures Vault Tiles (click to place)
				p.renderVaultSection(c, st)

				// 2. Contextual Active Placement Strip (only when a signature is selected)
				p.renderActivePlacementInspector(c, st)

				// 3. Contextual Export Banner (only when exporting, errored, or signed)
				p.renderExportBanner(c, st)
			})
		})
	})
}

// renderExportBanner renders export progress, error, or post-sign actions only when relevant.
func (p *Plugin) renderExportBanner(c *ui.Context, st *ViewState) {
	if !st.IsExporting && st.ExportError == "" && st.LastSignedFile == "" {
		return
	}

	ui.Column(c.Key("export-status-banner")).FillWidth().Gap(8).Padding(10).
		Background(theme.Ground).Border(1, theme.BorderQuiet).Children(func() {

		if st.IsExporting {
			msg := st.ExportStatus
			if msg == "" {
				msg = "Signing document..."
			}
			ui.Text(c, msg).FontSize(12).FontWeight(600).TextColor(theme.Ink)
		} else if st.LastSignedFile != "" {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(8, 8).Radius(999).Background(theme.Success)
				ui.Text(c, "Signed PDF Saved").FontSize(12).FontWeight(700).TextColor(theme.Success)
			})
			ui.Text(c, filepath.Base(st.LastSignedFile)).FontSize(11).TextColor(theme.InkMuted).SingleLine().Tooltip(st.LastSignedFile)
			ui.Row(c).FillWidth().Gap(6).Children(func() {
				components.SecondaryButton(c.Key("export-reveal-btn"), "Reveal in Folder", func() {
					go mygo.Shell.ShowItemInFolder(st.LastSignedFile)
				}).Height(28).Padding(0, 8).Grow(1)
				components.SecondaryButton(c.Key("export-open-btn"), "Open PDF", func() {
					go func() { _ = mygo.Shell.OpenPath(st.LastSignedFile) }()
				}).Height(28).Padding(0, 8).Grow(1)
			})
		}

		if st.ExportError != "" {
			ui.Box(c).FillWidth().Padding(6, 8).Background(theme.Paper).Border(1, theme.Danger).Children(func() {
				ui.Text(c, st.ExportError).FontSize(11).TextColor(theme.Danger)
			})
		}
	})
}

// renderVaultSection renders compact clickable signature tiles that place onto the active page.
func (p *Plugin) renderVaultSection(c *ui.Context, st *ViewState) {
	vault := p.Vault()
	items := vault.List()

	ui.Column(c).FillWidth().Gap(8).Children(func() {
		ui.Row(c).FillWidth().Justify(ui.SpaceBetween).AlignItems(ui.Center).Children(func() {
			ui.Text(c, p.t("pdfsign.vault_header")).FontSize(11).FontWeight(700).TextColor(theme.InkMuted)
			ui.Text(c, fmt.Sprintf("%d/%d", len(items), MaxSavedSignatures)).
				FontSize(11).FontWeight(600).FontFeatures("tnum").TextColor(theme.InkMuted)
		})

		if len(items) == 0 {
			ui.Box(c).FillWidth().Padding(16).Background(theme.Ground).Border(1, theme.BorderQuiet).Children(func() {
				ui.Text(c, p.t("pdfsign.vault_empty")).FontSize(12).TextColor(theme.InkMuted).TextAlign(ui.Center)
			})
			return
		}

		for _, item := range items {
			it := item
			bmp, hasBmp := p.getVaultBitmap(vault, it.ID)

			ui.Row(c.Key("panel-vault-"+it.ID)).
				FillWidth().Height(56).Gap(0).
				Background(theme.Paper).Border(1, theme.BorderQuiet).
				AlignItems(ui.Center).Children(func() {

				// Clickable & draggable signature preview tile -> click places centered or drag onto any page
				placeBtn := ui.ButtonBase(c.Key("vault-place-"+it.ID)).
					Grow(1).FillHeight().Padding(6, 10).Radius(0).
					Justify(ui.SpaceBetween).AlignItems(ui.Center).
					Cursor(ui.CursorPointer).
					Label(fmt.Sprintf("Place %s on page %d", it.Name, st.CurrentPage)).
					Tooltip("Click to place or drag onto a page")

				if placeBtn.Pressed() {
					placeBtn.Background(theme.SurfacePressed)
				} else if placeBtn.Hovered() {
					placeBtn.Background(theme.SurfaceHover)
				}
				if dx, dy, held := placeBtn.Dragged(); held {
					if st.DragPressID != it.ID {
						st.DragPressID = it.ID
						st.DragPressX = st.PointerX
						st.DragPressY = st.PointerY
					}
					if dx != 0 || dy != 0 {
						st.PointerX += dx
						st.PointerY += dy
						if !st.DragActive && math.Hypot(float64(st.PointerX-st.DragPressX), float64(st.PointerY-st.DragPressY)) > 4 {
							if img, err := vault.LoadImage(it.ID); err == nil {
								st.DragActive = true
								st.DragVaultID = it.ID
								st.DragImg = img
								st.DragBmp = bmp
							}
						}
						if st.DragActive {
							c.Invalidate()
						}
					}
				}
				if placeBtn.Clicked() && !st.DragActive {
					if img, err := vault.LoadImage(it.ID); err == nil {
						p.placeSignatureOnActivePage(img, it.ID)
						c.Invalidate()
					}
				}
				placeBtn.Children(func() {
					if hasBmp {
						ui.Image(c, bmp).MaxHeight(40).MaxWidth(150)
					} else {
						ui.Text(c, it.Name).FontSize(12).FontWeight(600)
					}
					ui.Text(c, "Place").FontSize(11).FontWeight(600).TextColor(theme.Accent)
				})

				// Vertical divider
				ui.Box(c).Width(1).Height(32).Background(theme.BorderQuiet)

				// Delete from vault button
				delBtn := ui.ButtonBase(c.Key("vault-del-"+it.ID)).
					Width(36).FillHeight().Radius(0).
					Justify(ui.Center).AlignItems(ui.Center).
					Cursor(ui.CursorPointer).
					Label("Delete saved signature").Tooltip("Delete saved signature")
				if delBtn.Hovered() {
					delBtn.Background(theme.SurfaceHover)
				}
				if delBtn.Clicked() {
					_ = vault.Delete(it.ID)
					p.evictVaultBitmap(it.ID)
					c.Invalidate()
				}
				delBtn.Children(func() {
					ui.Icon(c, components.IconClose).Size(12, 12).TextColor(theme.InkMuted)
				})
			})
		}
	})
}

// renderActivePlacementInspector renders a compact contextual strip for the currently selected signature.
func (p *Plugin) renderActivePlacementInspector(c *ui.Context, st *ViewState) {
	if st.SelectedID == "" {
		return
	}
	sig := st.findPlacement(st.SelectedID)
	if sig == nil {
		return
	}

	ui.Column(c.Key("active-sig-inspector")).FillWidth().Gap(8).Padding(10).
		Background(theme.Ground).Border(2, theme.Accent).Children(func() {

		ui.Row(c).FillWidth().Justify(ui.SpaceBetween).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "SELECTED").FontSize(11).FontWeight(700).TextColor(theme.Accent)
			ui.Text(c, fmt.Sprintf("Page %d", sig.Page)).FontSize(11).FontWeight(600).FontFeatures("tnum").TextColor(theme.InkMuted)
		})

		// Compact icon row: Lock/Unlock, Rotate, Duplicate, Delete
		ui.Row(c).FillWidth().Gap(6).Children(func() {
			lockIcon := components.IconUnlock
			lockTip := "Lock Position (L)"
			if sig.Locked {
				lockIcon = components.IconLock
				lockTip = "Unlock Position (L)"
			}
			lockBtn := ui.ButtonBase(c.Key("insp-lock-"+sig.ID)).
				Grow(1).Height(32).Radius(0).
				Border(1, theme.BorderQuiet).
				Justify(ui.Center).AlignItems(ui.Center).
				Cursor(ui.CursorPointer).Label(lockTip).Tooltip(lockTip)
			if sig.Locked {
				lockBtn.Background(theme.Accent).Border(1, theme.Ink).FocusRingColor(theme.Paper)
			} else {
				lockBtn.Background(theme.Paper)
				if lockBtn.Hovered() {
					lockBtn.Background(theme.SurfaceHover)
				}
			}
			if lockBtn.Clicked() {
				st.queueOp(opToggleLock, sig.ID)
				c.Invalidate()
			}
			lockBtn.Children(func() {
				icCol := theme.Ink
				if sig.Locked {
					icCol = theme.OnField
				}
				ui.Icon(c, lockIcon).Size(14, 14).TextColor(icCol)
			})

			if !sig.Locked {
				iconSquareBtn(c, "insp-rot-"+sig.ID, components.IconRotate, "Rotate 90 degrees", 32, false, func() {
					st.queueOp(opRotate90, sig.ID)
					c.Invalidate()
				})
			}

			iconSquareBtn(c, "insp-dup-"+sig.ID, components.IconCopy, p.t("pdfsign.btn_duplicate"), 32, false, func() {
				st.queueOp(opDuplicate, sig.ID)
				c.Invalidate()
			})

			if !sig.Locked {
				delBtn := ui.ButtonBase(c.Key("insp-del-"+sig.ID)).
					Width(32).Height(32).Radius(0).
					Background(theme.Danger).Border(1, theme.Ink).
					Justify(ui.Center).AlignItems(ui.Center).
					Cursor(ui.CursorPointer).FocusRingColor(theme.Paper).
					Label("Delete Signature").Tooltip("Delete Signature")
				if delBtn.Clicked() {
					st.queueOp(opDelete, sig.ID)
					c.Invalidate()
				}
				delBtn.Children(func() {
					ui.Icon(c, components.IconTrash).Size(14, 14).TextColor(theme.Paper)
				})
			}
		})

		if !sig.Locked {
			components.SecondaryButton(c.Key("insp-all-pages-"+sig.ID), p.t("pdfsign.btn_all_pages"), func() {
				st.queueOp(opApplyAllPages, sig.ID)
				c.Invalidate()
			}).Height(28).FillWidth()
		}
	})
}

func (p *Plugin) deletePlacedSignature(id string) {
	var next []PlacedSignature
	for _, s := range p.state.Placements {
		if s.ID != id {
			next = append(next, s)
		}
	}
	p.state.Placements = next
	if p.state.SelectedID == id {
		p.state.SelectedID = ""
	}
}

func defaultPlacementSize(img image.Image) (float64, float64) {
	bounds := img.Bounds()
	aspect := float64(bounds.Dx()) / float64(bounds.Dy())
	if aspect <= 0 {
		aspect = 2.0
	}
	sigW := 150.0
	sigH := sigW / aspect
	return sigW, sigH
}

// placeSignatureOnActivePage computes center coordinates and places the signature on the active page.
func (p *Plugin) placeSignatureOnActivePage(img image.Image, vaultID string) {
	st := p.state
	if st == nil || img == nil {
		return
	}

	sigW, sigH := defaultPlacementSize(img)
	pageDim := types.Dim{Width: 595, Height: 842}
	if st.CurrentPage-1 >= 0 && st.CurrentPage-1 < len(st.PageDims) {
		pageDim = st.PageDims[st.CurrentPage-1]
	}

	// Center signature horizontally and vertically on the target page
	centerX := (pageDim.Width - sigW) / 2
	centerY := (pageDim.Height - sigH) / 2
	p.placeSignatureAt(img, vaultID, st.CurrentPage, centerX, centerY)
}

func (p *Plugin) placeSignatureAt(img image.Image, vaultID string, page int, x, y float64) {
	st := p.state
	if st == nil || img == nil {
		return
	}

	sigW, sigH := defaultPlacementSize(img)
	pageDim := types.Dim{Width: 595, Height: 842}
	if page-1 >= 0 && page-1 < len(st.PageDims) {
		pageDim = st.PageDims[page-1]
	}

	if x < 0 {
		x = 0
	} else if maxX := pageDim.Width - sigW; maxX >= 0 && x > maxX {
		x = maxX
	}
	if y < 0 {
		y = 0
	} else if maxY := pageDim.Height - sigH; maxY >= 0 && y > maxY {
		y = maxY
	}

	newSig := PlacedSignature{
		ID:       fmt.Sprintf("sig-%d-%d", time.Now().UnixNano(), len(st.Placements)+1),
		SourceID: vaultID,
		Page:     page,
		X:        x,
		Y:        y,
		Width:    sigW,
		Height:   sigH,
		Opacity:  1.0,
		Crop:     DefaultNormalizedRect(),
		InkMode:  InkOriginal,
		Image:    img,
	}

	st.Placements = append(st.Placements, newSig)
	st.SelectedID = newSig.ID
	st.CurrentPage = page
}

func (p *Plugin) completeVaultDrop(c *ui.Context, st *ViewState) {
	if !st.DragActive || st.DragImg == nil {
		st.clearDrag()
		return
	}

	img := st.DragImg
	vaultID := st.DragVaultID
	px, py := st.PointerX, st.PointerY

	for pageNum := 1; pageNum <= st.PageCount; pageNum++ {
		h := st.PageCanvasHandles[pageNum]
		if h == nil {
			continue
		}
		pb := h.Bounds(c)
		if pb.W <= 0 || pb.H <= 0 {
			continue
		}
		if px >= pb.X && px <= pb.X+pb.W && py >= pb.Y && py <= pb.Y+pb.H {
			zoom := st.Zoom
			if zoom <= 0 {
				zoom = 1.0
			}
			sigW, sigH := defaultPlacementSize(img)
			dropX := float64(px-pb.X)/zoom - sigW/2
			dropY := float64(py-pb.Y)/zoom - sigH/2
			p.placeSignatureAt(img, vaultID, pageNum, dropX, dropY)
			break
		}
	}

	st.clearDrag()
	c.Invalidate()
}

// LoadPDF opens and inspects a PDF document.
func (p *Plugin) LoadPDF(pdfPath string) {
	st := p.state
	st.PDFPath = pdfPath
	st.PDFName = filepath.Base(pdfPath)
	st.Placements = nil
	st.SelectedID = ""
	st.CurrentPage = 1

	ctx := context.Background()
	count, err := p.Renderer().PageCount(ctx, pdfPath)
	if err != nil {
		st.RenderError = err.Error()
		return
	}
	st.PageCount = count

	// Query page dimensions
	st.PageDims = make([]types.Dim, count)
	for i := 1; i <= count; i++ {
		w, h, err := p.Renderer().PageDimensions(ctx, pdfPath, i)
		if err == nil {
			st.PageDims[i-1] = types.Dim{Width: w, Height: h}
		} else {
			st.PageDims[i-1] = types.Dim{Width: 595, Height: 842}
		}
	}

	// Preload the first page
	p.requestPageRender(1)
}

func (p *Plugin) ensureRenderCoordLocked() {
	if p.inFlightRenders == nil {
		p.inFlightRenders = make(map[PageCacheKey]bool)
	}
	if p.renderSem == nil {
		p.renderSem = make(chan struct{}, 2)
	}
	if p.vaultBitmaps == nil {
		p.vaultBitmaps = make(map[string]*ui.Bitmap)
	}
}

func (p *Plugin) getVaultBitmap(vault *SignatureVault, id string) (*ui.Bitmap, bool) {
	p.renderMu.Lock()
	p.ensureRenderCoordLocked()
	bmp, ok := p.vaultBitmaps[id]
	p.renderMu.Unlock()
	if ok && bmp != nil {
		return bmp, true
	}
	img, err := vault.LoadImage(id)
	if err != nil || img == nil {
		return nil, false
	}
	bmp = ui.NewBitmap(img)
	p.renderMu.Lock()
	p.ensureRenderCoordLocked()
	p.vaultBitmaps[id] = bmp
	p.renderMu.Unlock()
	return bmp, true
}

func (p *Plugin) evictVaultBitmap(id string) {
	p.renderMu.Lock()
	if p.vaultBitmaps != nil {
		delete(p.vaultBitmaps, id)
	}
	p.renderMu.Unlock()
}

func (p *Plugin) bumpZoomGen() {
	p.renderMu.Lock()
	p.zoomGen++
	p.renderMu.Unlock()
}

func (p *Plugin) scheduleDebouncedZoomRender(delay time.Duration) {
	p.renderMu.Lock()
	if p.zoomDebounce != nil {
		p.zoomDebounce.Stop()
	}
	gen := p.zoomGen
	p.zoomDebounce = time.AfterFunc(delay, func() {
		p.renderMu.Lock()
		current := p.zoomGen == gen
		p.renderMu.Unlock()
		if !current {
			return
		}
		if p.state != nil && p.state.PDFPath != "" {
			p.requestPageRender(p.state.CurrentPage)
		}
	})
	p.renderMu.Unlock()
}

func (p *Plugin) requestPageRender(pageNum int) {
	st := p.state
	if st == nil || st.PDFPath == "" || pageNum < 1 || pageNum > st.PageCount {
		return
	}

	pdfPath := st.PDFPath
	scale := QuantizeRenderScale(st.Zoom)
	if _, ok := p.pageCache.Get(pdfPath, pageNum, scale); ok {
		return
	}

	key := PageCacheKey{
		PDFPath:   pdfPath,
		PageNum:   pageNum,
		Scale1000: int(math.Round(scale * 1000)),
	}

	p.renderMu.Lock()
	p.ensureRenderCoordLocked()
	if p.inFlightRenders[key] {
		p.renderMu.Unlock()
		return
	}
	p.inFlightRenders[key] = true
	gen := p.zoomGen
	sem := p.renderSem
	p.renderMu.Unlock()

	// Render asynchronously with deduplication and bounded worker concurrency
	go func() {
		defer func() {
			p.renderMu.Lock()
			delete(p.inFlightRenders, key)
			p.renderMu.Unlock()
		}()

		sem <- struct{}{}
		defer func() { <-sem }()

		p.renderMu.Lock()
		stale := p.zoomGen != gen
		p.renderMu.Unlock()
		if stale {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		img, err := p.Renderer().RenderPage(ctx, pdfPath, pageNum, scale)
		if err == nil && img != nil {
			p.pageCache.Put(pdfPath, pageNum, scale, img)
			p.requestRedraw()
		}
	}()
}

func (p *Plugin) promptExport(c *ui.Context) {
	st := p.state
	ext := filepath.Ext(st.PDFPath)
	base := st.PDFPath[:len(st.PDFPath)-len(ext)]
	defaultOut := base + "_signed.pdf"

	go func() {
		outFile, err := mygo.Dialog.Save(mygo.SaveDialogOptions{
			DefaultPath: defaultOut,
			Filters:     []mygo.FileFilter{{Name: "PDF Documents", Extensions: []string{"pdf"}}},
		})

		if err != nil || outFile == "" {
			return
		}

		job := SignJob{
			InFile:     st.PDFPath,
			OutFile:    outFile,
			Signatures: st.Placements,
		}

		st.ActiveJobID = p.enqueueJob(fmt.Sprintf("Sign %s", filepath.Base(outFile)), job)
		st.IsExporting = true
		st.ExportStatus = "Starting PDF signing..."
		st.ExportError = ""
		p.requestRedraw()
	}()
}

func mathMax(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
