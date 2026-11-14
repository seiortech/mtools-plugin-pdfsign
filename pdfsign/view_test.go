package pdfsign

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"

	"github.com/seiortech/mtools-sdk/job"
	plugin "github.com/seiortech/mtools-sdk/settings"
)

func TestPDFSignWorkspace(t *testing.T) {
	tempDir := t.TempDir()
	settingsFile := filepath.Join(tempDir, "settings.json")
	store, _ := plugin.NewJSONSettingsStore(settingsFile, PluginSettingsSchema())
	q := job.NewQueue(1, nil)
	p := NewPlugin(tempDir, store, q)

	tt := ui.NewTester(p.View, 1200, 800)
	tt.Frame()

	// 1. Verify Empty State
	if !tt.HasText(p.t("pdfsign.empty_title")) {
		t.Errorf("expected empty state title %q", p.t("pdfsign.empty_title"))
	}
	if !tt.HasText(p.t("pdfsign.btn_select_file")) {
		t.Errorf("expected select file button %q", p.t("pdfsign.btn_select_file"))
	}

	// 2. Load PDF Document
	pdfPath := filepath.Join(tempDir, "sample.pdf")
	if err := os.WriteFile(pdfPath, []byte(minimalPDF), 0644); err != nil {
		t.Fatalf("failed to write sample pdf: %v", err)
	}

	p.LoadPDF(pdfPath)
	testImg := createSolidTestImage(200, 300, ColorInkBlack)
	p.pageCache.Put(pdfPath, 1, 1.0, testImg)
	tt.Frame()

	// 3. Verify Loaded Document Interface in Top Bar & Signatures Rail
	if !tt.HasText("sample.pdf") {
		t.Errorf("expected document name 'sample.pdf' in top bar")
	}
	if !tt.HasText("Signatures") {
		t.Errorf("expected 'Signatures' right panel header")
	}
	if !tt.HasText("+ New") {
		t.Errorf("expected '+ New' signature button in right panel")
	}

	// 4. Test Signature Studio Modal
	p.state.StudioOpen = true
	p.state.StudioState = NewStudioState()
	tt.Frame()

	if !tt.HasText(p.t("pdfsign.studio_title")) {
		t.Errorf("expected studio modal title")
	}
	if !tt.HasText(p.t("pdfsign.tab_draw")) {
		t.Errorf("expected draw tab in studio")
	}
	if !tt.HasText(p.t("pdfsign.tab_photo")) {
		t.Errorf("expected photo tab in studio")
	}

	// 5. Test Signature Placement on Page (Centered Placement)
	testImg = createSolidTestImage(120, 60, ColorInkBlack)
	p.placeSignatureOnActivePage(testImg, "test-sig-id")
	p.state.StudioOpen = false
	tt.Frame()

	if len(p.state.Placements) != 1 {
		t.Fatalf("expected 1 placed signature, got %d", len(p.state.Placements))
	}
	if p.state.SelectedID != p.state.Placements[0].ID {
		t.Errorf("expected placed signature to be selected")
	}

	// Verify centered coordinates: page 1 dims are 200x300, sig is 150x75 (aspect 2.0)
	sig0 := p.state.Placements[0]
	if sig0.X <= 0 || sig0.Y <= 0 {
		t.Errorf("expected centered signature coordinates > 0, got X=%.1f, Y=%.1f", sig0.X, sig0.Y)
	}

	// 6. Test Duplicate Button on Inspector
	if !tt.HasText(p.t("pdfsign.btn_duplicate")) {
		t.Errorf("expected duplicate button on selected signature inspector")
	}
	if err := tt.Click(p.t("pdfsign.btn_duplicate")); err != nil {
		t.Fatalf("click duplicate failed: %v", err)
	}
	tt.Frame()

	if len(p.state.Placements) != 2 {
		t.Errorf("expected 2 placed signatures after duplicate, got %d", len(p.state.Placements))
	}

	// 7. Test Delete
	p.deletePlacedSignature(p.state.Placements[1].ID)
	if len(p.state.Placements) != 1 {
		t.Errorf("expected 1 placed signature after delete, got %d", len(p.state.Placements))
	}
}

// TestDeletePlacedSignatureDoesNotPanic reproduces the crash where deleting the 6th signature
// on canvas would trigger "panic: runtime error: index out of range [5] with length 5".
func TestDeletePlacedSignatureDoesNotPanic(t *testing.T) {
	tempDir := t.TempDir()
	settingsFile := filepath.Join(tempDir, "settings.json")
	store, _ := plugin.NewJSONSettingsStore(settingsFile, PluginSettingsSchema())
	q := job.NewQueue(1, nil)
	p := NewPlugin(tempDir, store, q)

	pdfPath := filepath.Join(tempDir, "multi_sig.pdf")
	if err := os.WriteFile(pdfPath, []byte(minimalPDF), 0644); err != nil {
		t.Fatalf("failed to write test pdf: %v", err)
	}
	p.LoadPDF(pdfPath)
	testImg := createSolidTestImage(100, 50, ColorInkBlack)
	p.pageCache.Put(pdfPath, 1, 1.0, testImg)

	// Place 6 signatures
	for i := 0; i < 6; i++ {
		p.placeSignatureOnActivePage(testImg, "vault-sig")
	}
	if len(p.state.Placements) != 6 {
		t.Fatalf("expected 6 placed signatures, got %d", len(p.state.Placements))
	}

	// Select the 6th signature (index 5)
	lastID := p.state.Placements[5].ID
	p.state.SelectedID = lastID

	tt := ui.NewTester(p.View, 1200, 800)
	tt.Frame()

	// Click delete button in selected signature inspector
	if !tt.HasText("Delete Signature") {
		t.Fatalf("expected 'Delete Signature' button in inspector")
	}
	if err := tt.Click("Delete Signature"); err != nil {
		t.Fatalf("click delete button failed: %v", err)
	}

	// Next frame applies deferred mutation; must not panic and must now have 5 signatures
	tt.Frame()
	if len(p.state.Placements) != 5 {
		t.Errorf("expected 5 placed signatures after delete, got %d", len(p.state.Placements))
	}
	for _, sig := range p.state.Placements {
		if sig.ID == lastID {
			t.Errorf("expected deleted signature %s to be gone", lastID)
		}
	}
}

// TestSignatureLockAndUnlock verifies that locking a signature prevents modification and can be unlocked.
func TestSignatureLockAndUnlock(t *testing.T) {
	tempDir := t.TempDir()
	settingsFile := filepath.Join(tempDir, "settings.json")
	store, _ := plugin.NewJSONSettingsStore(settingsFile, PluginSettingsSchema())
	p := NewPlugin(tempDir, store, nil)

	pdfPath := filepath.Join(tempDir, "test.pdf")
	_ = os.WriteFile(pdfPath, []byte(minimalPDF), 0644)
	p.LoadPDF(pdfPath)

	testImg := createSolidTestImage(100, 50, ColorInkBlack)
	p.placeSignatureOnActivePage(testImg, "vault-1")

	sig := &p.state.Placements[0]
	if sig.Locked {
		t.Errorf("expected newly placed signature to be unlocked")
	}

	// Toggle lock on
	p.state.queueOp(opToggleLock, sig.ID)
	p.applyPendingPlacementActions(p.state)

	sig = &p.state.Placements[0]
	if !sig.Locked {
		t.Errorf("expected signature to be locked after opToggleLock")
	}

	// Toggle lock off
	p.state.queueOp(opToggleLock, sig.ID)
	p.applyPendingPlacementActions(p.state)

	sig = &p.state.Placements[0]
	if sig.Locked {
		t.Errorf("expected signature to be unlocked after second opToggleLock")
	}
}

// TestDeselectOnEscape verifies pressing Escape clears the selected signature.
func TestDeselectOnEscape(t *testing.T) {
	tempDir := t.TempDir()
	settingsFile := filepath.Join(tempDir, "settings.json")
	store, _ := plugin.NewJSONSettingsStore(settingsFile, PluginSettingsSchema())
	p := NewPlugin(tempDir, store, nil)

	pdfPath := filepath.Join(tempDir, "test.pdf")
	_ = os.WriteFile(pdfPath, []byte(minimalPDF), 0644)
	p.LoadPDF(pdfPath)

	testImg := createSolidTestImage(100, 50, ColorInkBlack)
	p.pageCache.Put(pdfPath, 1, 1.0, testImg)
	p.placeSignatureOnActivePage(testImg, "vault-1")

	p.state.SelectedID = p.state.Placements[0].ID

	tt := ui.NewTester(p.View, 1200, 800)
	tt.Frame()

	if p.state.SelectedID == "" {
		t.Fatalf("expected signature to be selected")
	}

	// Press Escape
	tt.Key(0, ui.KeyEscape)
	tt.Frame()

	if p.state.SelectedID != "" {
		t.Errorf("expected SelectedID to be cleared after Escape, got %q", p.state.SelectedID)
	}
}

// TestPinchToZoomGesture verifies that trackpad pinch gestures smoothly adjust zoom.
func TestPinchToZoomGesture(t *testing.T) {
	tempDir := t.TempDir()
	settingsFile := filepath.Join(tempDir, "settings.json")
	store, _ := plugin.NewJSONSettingsStore(settingsFile, PluginSettingsSchema())
	p := NewPlugin(tempDir, store, nil)

	pdfPath := filepath.Join(tempDir, "test.pdf")
	_ = os.WriteFile(pdfPath, []byte(minimalPDF), 0644)
	p.LoadPDF(pdfPath)

	testImg := createSolidTestImage(100, 50, ColorInkBlack)
	p.pageCache.Put(pdfPath, 1, 1.0, testImg)

	tt := ui.NewTester(p.View, 1200, 800)
	tt.Frame()

	initZoom := p.state.Zoom
	// Pinch in: scale = 1.2
	tt.Pinch(300, 300, 1.2)
	tt.Frame()

	if p.state.Zoom <= initZoom {
		t.Errorf("expected zoom to increase after pinch, init=%v, got=%v", initZoom, p.state.Zoom)
	}

	// Verify nearest cached image lookup prevents white flash
	if _, ok := p.pageCache.GetNearest(pdfPath, 1); !ok {
		t.Errorf("expected GetNearest to find previously rendered image")
	}
}

// TestFinishAndSignRecordsGlobalHistory verifies completing a job records a HistoryItem and populates HistoryEntries.
func TestFinishAndSignRecordsGlobalHistory(t *testing.T) {
	tempDir := t.TempDir()
	settingsFile := filepath.Join(tempDir, "settings.json")
	store, _ := plugin.NewJSONSettingsStore(settingsFile, PluginSettingsSchema())
	q := job.NewQueue(1, nil)
	p := NewPlugin(tempDir, store, q)

	pdfPath := filepath.Join(tempDir, "sample.pdf")
	_ = os.WriteFile(pdfPath, []byte(minimalPDF), 0644)
	p.LoadPDF(pdfPath)

	testImg := createSolidTestImage(100, 50, ColorInkBlack)
	p.placeSignatureOnActivePage(testImg, "v1")

	outPath := filepath.Join(tempDir, "sample_signed.pdf")
	j := SignJob{
		InFile:     pdfPath,
		OutFile:    outPath,
		Signatures: p.state.Placements,
	}

	jobID := p.enqueueJob("Test Sign", j)
	p.state.ActiveJobID = jobID

	// Wait for queue worker to finish
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok := q.GetRecord(jobID); ok && (rec.Status == job.StatusSucceeded || rec.Status == job.StatusFailed) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Sync active job
	p.syncActiveJob(p.state)

	if p.state.IsExporting {
		t.Errorf("expected IsExporting to be false after completion")
	}
	if p.state.LastSignedFile != outPath {
		t.Errorf("expected LastSignedFile == %q, got %q", outPath, p.state.LastSignedFile)
	}

	// Verify plugin history store
	items := p.History().All()
	if len(items) != 1 {
		t.Fatalf("expected 1 history item, got %d", len(items))
	}
	if items[0].OutputFile != outPath {
		t.Errorf("expected history output file == %q, got %q", outPath, items[0].OutputFile)
	}

	// Verify tool.HistoryEntry for global history
	entries := p.HistoryEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 global history entry, got %d", len(entries))
	}
	if entries[0].FilePath != outPath {
		t.Errorf("expected entry FilePath == %q, got %q", outPath, entries[0].FilePath)
	}
}

// TestSimplifiedWorkspaceTopBarAndRail verifies the top bar controls, collapsible Signatures rail, and Studio block tabs.
func TestSimplifiedWorkspaceTopBarAndRail(t *testing.T) {
	tempDir := t.TempDir()
	settingsFile := filepath.Join(tempDir, "settings.json")
	store, _ := plugin.NewJSONSettingsStore(settingsFile, PluginSettingsSchema())
	p := NewPlugin(tempDir, store, nil)

	pdfPath := filepath.Join(tempDir, "contract.pdf")
	_ = os.WriteFile(pdfPath, []byte(minimalPDF), 0644)
	p.LoadPDF(pdfPath)

	testImg := createSolidTestImage(200, 300, ColorInkBlack)
	p.pageCache.Put(pdfPath, 1, 1.0, testImg)

	// Save a signature into the vault to verify clickable vault tile
	if _, err := p.Vault().SaveImage("Sig 1", "drawn", createSolidTestImage(100, 50, ColorInkBlack)); err != nil {
		t.Fatalf("save vault sig failed: %v", err)
	}

	tt := ui.NewTester(p.View, 1200, 800)
	tt.Frame()

	// Verify top bar elements
	if !tt.HasText("Open") || !tt.HasText("contract.pdf") || !tt.HasText("1 / 1") || !tt.HasText("100%") || !tt.HasText("Fit") {
		t.Errorf("expected unified top bar controls (Open, contract.pdf, 1 / 1, 100%%, Fit)")
	}

	// Click clickable vault tile ("Place")
	if !tt.HasText("Place") {
		t.Fatalf("expected clickable vault tile with 'Place' hint")
	}
	if err := tt.Click("Place"); err != nil {
		t.Fatalf("click vault tile failed: %v", err)
	}
	tt.Frame()
	if len(p.state.Placements) != 1 {
		t.Errorf("expected 1 placement after clicking vault tile, got %d", len(p.state.Placements))
	}

	// Collapse and expand right rail via top bar toggle
	if err := tt.Click("Collapse signatures panel"); err != nil {
		t.Fatalf("collapse rail failed: %v", err)
	}
	tt.Frame()
	if !p.state.RightPanelCollapsed {
		t.Errorf("expected RightPanelCollapsed to be true")
	}

	if err := tt.Click("Expand signatures panel"); err != nil {
		t.Fatalf("expand rail failed: %v", err)
	}
	tt.Frame()
	if p.state.RightPanelCollapsed {
		t.Errorf("expected RightPanelCollapsed to be false")
	}

	// Open Signature Studio and verify Bauhaus block tabs accessibility
	if err := tt.Click("+ New"); err != nil {
		t.Fatalf("click + New failed: %v", err)
	}
	tt.Frame()
	if _, ok := tt.AccessByRole(ui.RoleTab, p.t("pdfsign.tab_draw")); !ok {
		t.Errorf("expected accessible RoleTab %q in Signature Studio", p.t("pdfsign.tab_draw"))
	}
	if _, ok := tt.AccessByRole(ui.RoleTab, p.t("pdfsign.tab_photo")); !ok {
		t.Errorf("expected accessible RoleTab %q in Signature Studio", p.t("pdfsign.tab_photo"))
	}
}

func TestPageBitmapCacheNearestAndBitmapReuse(t *testing.T) {
	cache := NewPageBitmapCache(10)
	img1 := createSolidTestImage(100, 100, ColorInkBlack)
	img2 := createSolidTestImage(200, 200, ColorInkBlack)

	cache.Put("doc.pdf", 1, 1.0, img1)
	cache.Put("doc.pdf", 1, 2.0, img2)

	bmp1a, ok := cache.GetBitmap("doc.pdf", 1, 1.0)
	if !ok || bmp1a == nil {
		t.Fatalf("expected GetBitmap hit at 1.0")
	}
	bmp1b, ok := cache.GetBitmap("doc.pdf", 1, 1.0)
	if !ok || bmp1a != bmp1b {
		t.Errorf("expected GetBitmap to reuse cached *ui.Bitmap pointer")
	}

	nearest, ok := cache.GetNearestBitmap("doc.pdf", 1, 1.85)
	if !ok || nearest == nil {
		t.Fatalf("expected GetNearestBitmap hit")
	}
	bmp2, _ := cache.GetBitmap("doc.pdf", 1, 2.0)
	if nearest != bmp2 {
		t.Errorf("expected GetNearestBitmap(1.85) to return 2.0x bitmap rather than 1.0x bitmap")
	}
}

type countingRenderer struct {
	calls atomic.Int32
	img   image.Image
}

func (r *countingRenderer) PageCount(ctx context.Context, pdfPath string) (int, error) {
	return 1, nil
}

func (r *countingRenderer) PageDimensions(ctx context.Context, pdfPath string, pageNum int) (float64, float64, error) {
	return 595, 842, nil
}

func (r *countingRenderer) RenderPage(ctx context.Context, pdfPath string, pageNum int, scale float64) (image.Image, error) {
	r.calls.Add(1)
	time.Sleep(40 * time.Millisecond)
	return r.img, nil
}

func TestRequestPageRenderDeduplication(t *testing.T) {
	tempDir := t.TempDir()
	p := NewPlugin(tempDir, nil, nil)
	cr := &countingRenderer{img: createSolidTestImage(100, 100, ColorInkBlack)}
	p.SetRenderer(cr)

	p.state.PDFPath = "test.pdf"
	p.state.PageCount = 1
	p.state.Zoom = 1.02

	for i := 0; i < 8; i++ {
		p.requestPageRender(1)
	}

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := p.pageCache.Get("test.pdf", 1, QuantizeRenderScale(p.state.Zoom)); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := cr.calls.Load(); got != 1 {
		t.Errorf("expected deduplicated RenderPage calls == 1, got %d", got)
	}
}

func TestScrollToPagePreservesTargetDuringSmoothGlide(t *testing.T) {
	tempDir := t.TempDir()
	p := NewPlugin(tempDir, nil, nil)
	p.state.PDFPath = "multi.pdf"
	p.state.PageCount = 4
	p.state.CurrentPage = 1
	p.state.Zoom = 1.0
	p.state.PageDims = []types.Dim{
		{Width: 595, Height: 842},
		{Width: 595, Height: 842},
		{Width: 595, Height: 842},
		{Width: 595, Height: 842},
	}

	p.scrollToPage(p.state, 3)
	if !p.state.Scroll.Scrolling() {
		t.Fatalf("expected Scroll.Scrolling() to be true after scrollToPage")
	}

	// Simulate an animation frame while Scroll.Y is still at 0 (page 1)
	p.state.Scroll.Y = 0
	p.syncCurrentPageFromScroll(p.state)

	if p.state.CurrentPage != 3 {
		t.Errorf("expected CurrentPage to stay 3 during smooth scroll glide, got %d", p.state.CurrentPage)
	}
}

func TestVaultDragAndDropOntoCanvas(t *testing.T) {
	tmpDir := t.TempDir()
	p := NewPlugin(tmpDir, nil, nil)
	testImg := createSolidTestImage(120, 60, ColorInkBlack)
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, testImg); err != nil {
		t.Fatalf("png.Encode failed: %v", err)
	}
	saved, err := p.Vault().Save("Drag Sig", "draw", 120, 60, pngBuf.Bytes())
	if err != nil {
		t.Fatalf("Vault.Save failed: %v", err)
	}

	pdfPath := filepath.Join(tmpDir, "doc.pdf")
	if err := os.WriteFile(pdfPath, []byte(minimalPDF), 0644); err != nil {
		t.Fatalf("failed to write sample pdf: %v", err)
	}
	p.LoadPDF(pdfPath)
	p.pageCache.Put(pdfPath, 1, 1.0, createSolidTestImage(200, 300, ColorInkBlack))

	var pb ui.Rect
	tt := ui.NewTester(func(c *ui.Context) {
		p.View(c)
		if h := p.state.PageCanvasHandles[1]; h != nil {
			pb = h.Bounds(c)
		}
	}, 1200, 800)
	tt.Frame()

	// Locate the saved signature tile and the rendered page canvas
	tb, ok := tt.Find("Place Drag Sig on page 1")
	if !ok {
		t.Fatalf("expected vault tile to be rendered")
	}
	if pb.W <= 0 || pb.H <= 0 {
		t.Fatalf("expected page canvas bounds > 0, got %+v", pb)
	}

	startX := tb.X + tb.W/2
	startY := tb.Y + tb.H/2
	dropX := pb.X + pb.W/2
	dropY := pb.Y + pb.H/2

	tt.Press(startX, startY)
	tt.Move(startX-20, startY)
	tt.Move(dropX, dropY)
	if !p.state.DragActive {
		t.Fatalf("expected DragActive to be true while dragging vault tile")
	}
	tt.Release(dropX, dropY)

	if p.state.DragActive {
		t.Errorf("expected DragActive to clear after drop")
	}
	if len(p.state.Placements) != 1 {
		t.Fatalf("expected 1 placed signature after drop, got %d", len(p.state.Placements))
	}
	got := p.state.Placements[0]
	if got.SourceID != saved.ID {
		t.Errorf("expected SourceID %q, got %q", saved.ID, got.SourceID)
	}
	if p.state.SelectedID != got.ID {
		t.Errorf("expected dropped signature %q to be selected, got %q", got.ID, p.state.SelectedID)
	}

	// Cancel drag when released outside any page canvas
	tt.Press(startX, startY)
	tt.Move(startX-20, startY)
	tt.Move(5, 5)
	tt.Release(5, 5)
	if p.state.DragActive {
		t.Errorf("expected DragActive to clear after outside release")
	}
	if len(p.state.Placements) != 1 {
		t.Errorf("expected no extra placement when dropped outside canvas, got %d", len(p.state.Placements))
	}
}
