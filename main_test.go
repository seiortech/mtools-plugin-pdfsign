package main

import (
	"bufio"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seiortech/mtools-plugin-pdfsign/internal/pdfsign"
	"github.com/seiortech/mtools-sdk/pluginapi"
)

type fakeSignRenderer struct {
	pages       int
	posterPage  int
	hetero      bool
	allCalls    atomic.Int32
	dimCalls    atomic.Int32
	renderCalls atomic.Int32
}

func (f *fakeSignRenderer) PageCount(context.Context, string) (int, error) {
	return f.pages, nil
}

func (f *fakeSignRenderer) PageDimensions(context.Context, string, int) (float64, float64, error) {
	f.dimCalls.Add(1)
	return 595, 842, nil
}

func (f *fakeSignRenderer) AllPageDimensions(context.Context, string) ([]pdfsign.PageDim, error) {
	f.allCalls.Add(1)
	out := make([]pdfsign.PageDim, f.pages)
	for i := range out {
		switch {
		case i+1 == f.posterPage:
			out[i] = pdfsign.PageDim{Width: 3000, Height: 4000}
		case f.hetero && i%2 == 1:
			out[i] = pdfsign.PageDim{Width: 300, Height: 400}
		default:
			out[i] = pdfsign.PageDim{Width: 595, Height: 842}
		}
	}
	return out, nil
}

func (f *fakeSignRenderer) RenderPage(_ context.Context, _ string, pageNum int, _ float64) (image.Image, error) {
	f.renderCalls.Add(1)
	img := image.NewRGBA(image.Rect(0, 0, 40, 60))
	c := color.RGBA{R: uint8(pageNum % 255), G: 110, B: 90, A: 255}
	for y := 0; y < 60; y++ {
		for x := 0; x < 40; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img, nil
}

type signHarness struct {
	t         *testing.T
	inWrite   *io.PipeWriter
	done      chan error
	mu        sync.Mutex
	trees     []pluginapi.UITree
	revOrder  bool
	closed    bool
	drainDone chan struct{}
}

func startSignHarness(t *testing.T, assetsDir, dataDir string, srv *pdfSignServer, settings map[string]string) *signHarness {
	t.Helper()
	t.Setenv("MTOOLS_UI_ASSETS", assetsDir)
	inRead, inWrite := io.Pipe()
	outRead, outWrite := io.Pipe()
	id := pluginapi.InitializeResult{Protocol: pluginapi.Protocol, PluginID: "com.mtools.pdfsign", Version: "1.0.0"}

	done := make(chan error, 1)
	go func() {
		done <- pluginapi.ServeLive(context.Background(), inRead, outWrite, id, srv, srv)
		_ = outWrite.Close()
	}()

	h := &signHarness{
		t:         t,
		inWrite:   inWrite,
		done:      done,
		revOrder:  true,
		drainDone: make(chan struct{}),
	}

	send := func(m pluginapi.Message) {
		t.Helper()
		if err := pluginapi.WriteMessage(inWrite, m); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}
	reader := bufio.NewReader(outRead)
	recv := func() pluginapi.Message {
		t.Helper()
		m, err := pluginapi.ReadMessage(reader)
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		return m
	}

	initRaw, _ := json.Marshal(id)
	send(pluginapi.Message{JSONRPC: "2.0", ID: 1, Method: pluginapi.MethodInitialize, Params: initRaw})
	if r := recv(); r.Error != nil {
		t.Fatalf("init error: %+v", r.Error)
	}

	merged := map[string]string{"plugin_data_dir": dataDir}
	for k, v := range settings {
		merged[k] = v
	}
	openRaw, _ := json.Marshal(pluginapi.UIOpenParams{
		Tool:     "pdfsign",
		Viewport: pluginapi.UIViewport{Width: 1200, Height: 800},
		Settings: merged,
	})
	send(pluginapi.Message{JSONRPC: "2.0", ID: 2, Method: pluginapi.MethodUIOpen, Params: openRaw})
	r2 := recv()
	if r2.Error != nil {
		t.Fatalf("open error: %+v", r2.Error)
	}
	tr, err := pluginapi.DecodeTree(r2.Result)
	if err != nil {
		t.Fatalf("DecodeTree: %v", err)
	}
	h.trees = append(h.trees, tr)

	go func() {
		defer close(h.drainDone)
		lastRev := tr.Rev
		for {
			m, err := pluginapi.ReadMessage(reader)
			if err != nil {
				return
			}
			if m.Method == pluginapi.MethodUITree {
				tr, err := pluginapi.DecodeTree(m.Params)
				if err != nil {
					h.mu.Lock()
					h.revOrder = false
					h.mu.Unlock()
					continue
				}
				h.mu.Lock()
				if tr.Rev <= lastRev {
					h.revOrder = false
				}
				lastRev = tr.Rev
				h.trees = append(h.trees, tr)
				h.mu.Unlock()
			}
		}
	}()
	return h
}

func (h *signHarness) sendEvent(ev pluginapi.UIEvent) {
	h.t.Helper()
	raw, _ := json.Marshal(ev)
	if err := pluginapi.WriteMessage(h.inWrite, pluginapi.Message{
		JSONRPC: "2.0", Method: pluginapi.MethodUIEvent, Params: raw,
	}); err != nil {
		h.t.Fatalf("sendEvent: %v", err)
	}
}

func (h *signHarness) close() {
	if h.closed {
		return
	}
	h.closed = true
	raw, _ := json.Marshal(pluginapi.UICloseParams{Tool: "pdfsign"})
	_ = pluginapi.WriteMessage(h.inWrite, pluginapi.Message{
		JSONRPC: "2.0", Method: pluginapi.MethodUIClose, Params: raw,
	})
	_ = h.inWrite.Close()
	if err := <-h.done; err != nil {
		h.t.Fatalf("ServeLive exit error: %v", err)
	}
	<-h.drainDone
}

func countTreeNodes(n pluginapi.UINode) int {
	total := 1
	for _, ch := range n.Children {
		total += countTreeNodes(ch)
	}
	return total
}

func TestPDFSignSurfacesValidation(t *testing.T) {
	raw, err := os.ReadFile("surfaces.json")
	if err != nil {
		t.Fatalf("read surfaces.json: %v", err)
	}
	m := pluginapi.Manifest{
		Schema:       1,
		ID:           "com.mtools.pdfsign",
		Name:         "PDF Signature",
		Version:      "1.0.0",
		Platform:     "darwin-arm64",
		Publisher:    pluginapi.Publisher{Name: "mtools Team", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		Capabilities: []pluginapi.Capability{pluginapi.Process, pluginapi.FilesRead, pluginapi.FilesWrite, pluginapi.UILive, pluginapi.Network},
		Entry:        pluginapi.Entry{Kind: "sidecar", Path: "bin/tool", Protocol: pluginapi.Protocol},
		Tools:        []pluginapi.Tool{{ID: "pdfsign", Name: "PDF Signature", Description: "Sign PDFs", Category: "Document"}},
		Files:        map[string]pluginapi.FileInfo{"bin/tool": {SHA256: strings.Repeat("a", 64), Size: 10, Executable: true}},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validate: %v", err)
	}
	doc, err := pluginapi.DecodeSurfaces(raw, m)
	if err != nil {
		t.Fatalf("DecodeSurfaces: %v", err)
	}
	if len(doc.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(doc.Tools))
	}
	var found bool
	for _, s := range doc.Tools[0].Settings {
		if s.ID == "default_output_dir" {
			found = true
			if s.Default != "" || s.Required {
				t.Fatalf("expected default_output_dir default=\"\" required=false, got default=%q required=%v", s.Default, s.Required)
			}
		}
	}
	if !found {
		t.Fatal("default_output_dir setting missing from surfaces.json")
	}
}

func TestPDFSignLiveUILargeDocSpacersAnd8192Clamp(t *testing.T) {
	dir := t.TempDir()
	fakePDF := filepath.Join(dir, "giant.pdf")
	if err := os.WriteFile(fakePDF, []byte("%PDF-1.4"), 0600); err != nil {
		t.Fatal(err)
	}

	srv := newPDFSignServer()
	defer func() { _ = srv.Close(context.Background()) }()
	fr := &fakeSignRenderer{pages: 1200, posterPage: 600}
	srv.renderer = fr

	if _, err := srv.Open(context.Background(), pluginapi.UIOpenParams{
		Tool:     "pdfsign",
		Viewport: pluginapi.UIViewport{Width: 1200, Height: 800},
		Settings: map[string]string{"plugin_data_dir": t.TempDir()},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := srv.Event(context.Background(), pluginapi.UIEvent{
		Rev: 1, Node: "empty_open_pdf", Kind: pluginapi.UIEventChange, Value: fakePDF,
	}, nil); err != nil {
		t.Fatal(err)
	}

	if fr.allCalls.Load() != 1 || fr.dimCalls.Load() != 0 {
		t.Fatalf("expected 1 AllPageDimensions call, got all=%d dim=%d", fr.allCalls.Load(), fr.dimCalls.Load())
	}

	// Jump to giant 3000x4000pt poster at page 600 with 3.0x zoom (unclamped would be 9000x12000 > 8192!).
	srv.mu.Lock()
	srv.zoom = 3.0
	srv.jumpToPageLocked(600, nil)
	tree := srv.buildTreeLocked()
	srv.mu.Unlock()

	if err := tree.Validate(); err != nil {
		t.Fatalf("1200-page + poster tree failed Validate(): %v", err)
	}
	if n := countTreeNodes(tree.Root); n > 600 {
		t.Fatalf("expected chunked spacers to keep node count well below MaxUINodes (2048), got %d", n)
	}
}

// minimalValid1x1WebP is a valid 1x1 lossless VP8L WebP image (black opaque pixel).
var minimalValid1x1WebP = []byte{
	'R', 'I', 'F', 'F', 0x24, 0x00, 0x00, 0x00,
	'W', 'E', 'B', 'P', 'V', 'P', '8', 'L',
	0x17, 0x00, 0x00, 0x00, 0x2f, 0x00, 0x00, 0x00,
	0x00, 0x07, 0x10, 0x00, 0x00, 0x88, 0x88, 0x08,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
}

func TestPDFSignLiveUIStudioWebPAndMemoryRelease(t *testing.T) {
	assetsDir := t.TempDir()
	dataDir := t.TempDir()
	imgDir := t.TempDir()

	// 1. Oversized 2600x1200 PNG to test 2048px rawPhoto cap & studio cleanup.
	bigPNG := filepath.Join(imgDir, "sig_photo.png")
	src := image.NewNRGBA(image.Rect(0, 0, 2600, 1200))
	for y := 400; y < 800; y++ {
		for x := 600; x < 2000; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 20, G: 20, B: 20, A: 255})
		}
	}
	f, err := os.Create(bigPNG)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, src); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// 2. WebP file to prove golang.org/x/image/webp is registered in live_studio.go.
	webpPath := filepath.Join(imgDir, "tiny.webp")
	if err := os.WriteFile(webpPath, minimalValid1x1WebP, 0600); err != nil {
		t.Fatal(err)
	}
	wf, err := os.Open(webpPath)
	if err != nil {
		t.Fatal(err)
	}
	_, format, cfgErr := image.DecodeConfig(wf)
	_ = wf.Close()
	if cfgErr != nil || format != "webp" {
		t.Fatalf("expected webp decoder registered, got format=%q err=%v", format, cfgErr)
	}

	srv := newPDFSignServer()
	defer func() { _ = srv.Close(context.Background()) }()
	h := startSignHarness(t, assetsDir, dataDir, srv, nil)
	defer h.close()

	h.sendEvent(pluginapi.UIEvent{Rev: 1, Node: "vault_photo_btn", Kind: pluginapi.UIEventClick})
	h.sendEvent(pluginapi.UIEvent{Rev: 2, Node: "studio_choose_photo", Kind: pluginapi.UIEventChange, Value: bigPNG})

	deadline := time.Now().Add(3 * time.Second)
	var rawBounds image.Rectangle
	var previewFile string
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		if srv.studio.rawPhoto != nil {
			rawBounds = srv.studio.rawPhoto.Bounds()
			previewFile = srv.studio.photoPreviewFile
		}
		srv.mu.Unlock()
		if previewFile != "" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if previewFile == "" {
		t.Fatal("expected studio photo preview file to be generated")
	}
	if rawBounds.Dx() > maxStudioRawPhotoDim || rawBounds.Dy() > maxStudioRawPhotoDim {
		t.Fatalf("expected rawPhoto capped <=%dpx, got %dx%d", maxStudioRawPhotoDim, rawBounds.Dx(), rawBounds.Dy())
	}

	// Cancel studio and verify buffers + temp preview file are freed.
	h.sendEvent(pluginapi.UIEvent{Rev: 3, Node: "studio_cancel", Kind: pluginapi.UIEventClick})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		closed := !srv.studio.open && srv.studio.rawPhoto == nil && srv.studio.photoThumb == nil && srv.studio.photoResult == nil
		srv.mu.Unlock()
		if closed {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	srv.mu.Lock()
	if srv.studio.rawPhoto != nil || srv.studio.photoThumb != nil || srv.studio.photoResult != nil || srv.studio.drawCanvas != nil {
		srv.mu.Unlock()
		t.Fatal("studio buffers were not released on cancel")
	}
	srv.mu.Unlock()

	if _, err := os.Stat(filepath.Join(assetsDir, previewFile)); !os.IsNotExist(err) {
		t.Fatalf("expected studio preview file %s removed on cancel", previewFile)
	}
}

func TestPDFSignLiveUIPhotoDebounceRenderMuRace(t *testing.T) {
	assetsDir := t.TempDir()
	dataDir := t.TempDir()
	photoPath := filepath.Join(t.TempDir(), "photo.png")
	img := image.NewNRGBA(image.Rect(0, 0, 200, 100))
	for y := 20; y < 80; y++ {
		for x := 30; x < 170; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 25, G: 25, B: 25, A: 255})
		}
	}
	f, err := os.Create(photoPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = png.Encode(f, img)
	_ = f.Close()

	srv := newPDFSignServer()
	defer func() { _ = srv.Close(context.Background()) }()
	h := startSignHarness(t, assetsDir, dataDir, srv, nil)

	h.sendEvent(pluginapi.UIEvent{Rev: 1, Node: "vault_photo_btn", Kind: pluginapi.UIEventClick})
	h.sendEvent(pluginapi.UIEvent{Rev: 2, Node: "studio_choose_photo", Kind: pluginapi.UIEventChange, Value: photoPath})
	time.Sleep(30 * time.Millisecond)

	// Interleave rapid slider debounce events with synchronous clicks to stress renderMu under -race.
	for i := 0; i < 8; i++ {
		h.sendEvent(pluginapi.UIEvent{Rev: uint64(3 + i*2), Node: "studio_threshold", Kind: pluginapi.UIEventChange, Value: "0.75"})
		time.Sleep(45 * time.Millisecond)
		h.sendEvent(pluginapi.UIEvent{Rev: uint64(4 + i*2), Node: "studio_photo_crisp", Kind: pluginapi.UIEventClick})
	}
	time.Sleep(120 * time.Millisecond)
	h.close()

	h.mu.Lock()
	okOrder := h.revOrder
	count := len(h.trees)
	h.mu.Unlock()
	if !okOrder {
		t.Fatal("detected out-of-order Rev or invalid UITree during concurrent photo debounce")
	}
	if count < 4 {
		t.Fatalf("expected multiple pushed trees, got %d", count)
	}
}

func TestPDFSignLiveUIAllPagesDedupAndLockGuard(t *testing.T) {
	assetsDir := t.TempDir()
	dataDir := t.TempDir()
	fakePDF := filepath.Join(t.TempDir(), "hetero.pdf")
	if err := os.WriteFile(fakePDF, []byte("%PDF-1.4"), 0600); err != nil {
		t.Fatal(err)
	}

	srv := newPDFSignServer()
	defer func() { _ = srv.Close(context.Background()) }()
	srv.renderer = &fakeSignRenderer{pages: 20, hetero: true}
	h := startSignHarness(t, assetsDir, dataDir, srv, nil)
	defer h.close()

	h.sendEvent(pluginapi.UIEvent{Rev: 1, Node: "empty_open_pdf", Kind: pluginapi.UIEventChange, Value: fakePDF})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		pc := srv.pageCount
		srv.mu.Unlock()
		if pc == 20 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Place a signature near the right edge of page 1 (595x842) and apply to all 20 pages
	// (even pages are 300x400 so clamping is exercised).
	sigImg := image.NewNRGBA(image.Rect(0, 0, 140, 60))
	for y := 0; y < 60; y++ {
		for x := 0; x < 140; x++ {
			sigImg.SetNRGBA(x, y, color.NRGBA{R: 20, G: 20, B: 20, A: 255})
		}
	}
	srv.mu.Lock()
	srv.placeSignatureAtLocked(sigImg, "", 1, 450, 750, srv.sess)
	srv.mu.Unlock()

	h.sendEvent(pluginapi.UIEvent{Rev: 2, Node: "insp_all_pages", Kind: pluginapi.UIEventClick})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		n := len(srv.placements)
		srv.mu.Unlock()
		if n == 20 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	srv.mu.Lock()
	if len(srv.placements) != 20 {
		srv.mu.Unlock()
		t.Fatalf("expected 20 placements, got %d", len(srv.placements))
	}
	if len(srv.visualAssets) != 1 {
		srv.mu.Unlock()
		t.Fatalf("expected 1 deduplicated visual asset for 20 identical placements, got %d", len(srv.visualAssets))
	}
	for _, pl := range srv.placements {
		dims := srv.pageDimsAtLocked(pl.Page)
		if pl.X < 0 || pl.Y < 0 || pl.X+pl.Width > dims.W+0.01 || pl.Y+pl.Height > dims.H+0.01 {
			srv.mu.Unlock()
			t.Fatalf("placement on page %d out of page bounds (%.0fx%.0f): %+v", pl.Page, dims.W, dims.H, pl)
		}
	}
	activeID := srv.activeID
	origX, origY, origRot, origInk, origCrisp := srv.placements[0].X, srv.placements[0].Y, srv.placements[0].Rotation, srv.placements[0].InkMode, srv.placements[0].Crisp
	srv.mu.Unlock()

	entries, err := os.ReadDir(assetsDir)
	if err != nil {
		t.Fatal(err)
	}
	placedFiles := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "placed-") {
			placedFiles++
		}
	}
	if placedFiles != 1 {
		t.Fatalf("expected 1 placed-*.png file on disk for 20 identical placements, got %d", placedFiles)
	}

	// Lock active placement and verify move, resize, rotate, recolor, crisp, and delete are all blocked.
	h.sendEvent(pluginapi.UIEvent{Rev: 3, Node: "sel_lock", Kind: pluginapi.UIEventClick})
	h.sendEvent(pluginapi.UIEvent{Rev: 4, Node: "placed_" + activeID, Kind: pluginapi.UIEventDragEnd, Value: "10,10"})
	h.sendEvent(pluginapi.UIEvent{Rev: 5, Node: "placed_" + activeID, Kind: pluginapi.UIEventResize, Value: "10,10,50,30"})
	h.sendEvent(pluginapi.UIEvent{Rev: 6, Node: "sel_rot_right", Kind: pluginapi.UIEventClick})
	h.sendEvent(pluginapi.UIEvent{Rev: 7, Node: "insp_ink_blue", Kind: pluginapi.UIEventClick})
	h.sendEvent(pluginapi.UIEvent{Rev: 8, Node: "insp_crisp", Kind: pluginapi.UIEventClick})
	h.sendEvent(pluginapi.UIEvent{Rev: 9, Node: "sel_del", Kind: pluginapi.UIEventClick})
	time.Sleep(60 * time.Millisecond)

	srv.mu.Lock()
	pl0 := srv.placements[0]
	totalPl := len(srv.placements)
	srv.mu.Unlock()
	if totalPl != 20 || !pl0.Locked || pl0.X != origX || pl0.Y != origY || pl0.Rotation != origRot || pl0.InkMode != origInk || pl0.Crisp != origCrisp {
		t.Fatalf("locked placement was mutated: total=%d pl0=%+v", totalPl, pl0)
	}
}

func TestPDFSignLiveUISettingsDefaultOutputDirAndClose(t *testing.T) {
	assetsDir := t.TempDir()
	dataDir := t.TempDir()
	pdfDir := t.TempDir()
	customOutDir := t.TempDir()
	fakePDF := filepath.Join(pdfDir, "contract.pdf")
	if err := os.WriteFile(fakePDF, []byte("%PDF-1.4"), 0600); err != nil {
		t.Fatal(err)
	}

	srv := newPDFSignServer()
	srv.renderer = &fakeSignRenderer{pages: 2}
	h := startSignHarness(t, assetsDir, dataDir, srv, map[string]string{"default_output_dir": ""})

	h.sendEvent(pluginapi.UIEvent{Rev: 1, Node: "empty_open_pdf", Kind: pluginapi.UIEventChange, Value: fakePDF})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		ok := srv.pdfPath == fakePDF
		srv.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Empty default_output_dir -> falls back to input PDF's folder.
	srv.mu.Lock()
	gotFallback := srv.defaultSignedOutputPathLocked()
	srv.mu.Unlock()
	wantFallback := filepath.Join(pdfDir, "contract-signed.pdf")
	if gotFallback != wantFallback {
		t.Fatalf("empty default_output_dir path = %q, want %q", gotFallback, wantFallback)
	}

	// Write settings.json with custom default_output_dir and trigger live settings reload.
	raw, _ := json.Marshal(map[string]string{"default_output_dir": customOutDir})
	if err := os.WriteFile(filepath.Join(dataDir, "settings.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	h.sendEvent(pluginapi.UIEvent{Rev: 2, Node: "settings", Kind: pluginapi.UIEventChange})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		d := srv.defaultOutputDir
		srv.mu.Unlock()
		if d == customOutDir {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	srv.mu.Lock()
	gotCustom := srv.defaultSignedOutputPathLocked()
	srv.mu.Unlock()
	wantCustom := filepath.Join(customOutDir, "contract-signed.pdf")
	if gotCustom != wantCustom {
		t.Fatalf("reloaded default_output_dir path = %q, want %q", gotCustom, wantCustom)
	}

	h.close()
	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entries, err := os.ReadDir(assetsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected all assets cleaned up on Close(), found %d entries", len(entries))
	}
}
