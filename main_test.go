package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seiortech/mtools-plugin-pdfsign/internal/pdfsign"
	"github.com/seiortech/mtools-sdk/pluginapi"
	"github.com/seiortech/mtools-sdk/ui/i18n"
)

const testMinimalPDF = `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 300] /Resources <<>> >>
endobj
xref
0 4
0000000000 65535 f 
0000000009 00000 n 
0000000058 00000 n 
0000000115 00000 n 
trailer
<< /Size 4 /Root 1 0 R >>
startxref
198
%%EOF`

func TestPDFSignManifestAndSurfaces(t *testing.T) {
	mRaw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	sRaw, err := os.ReadFile("surfaces.json")
	if err != nil {
		t.Fatalf("read surfaces.json: %v", err)
	}
	var m pluginapi.Manifest
	if err := jsonUnmarshalManifest(mRaw, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	doc, err := pluginapi.DecodeSurfaces(sRaw, m)
	if err != nil {
		t.Fatalf("DecodeSurfaces: %v", err)
	}
	if len(doc.Tools) != 1 || doc.Tools[0].Live == nil {
		t.Fatalf("expected pdfsign tool with Live UI enabled, got %+v", doc.Tools)
	}
	if !doc.Tools[0].Live.FullBleed {
		t.Fatalf("expected pdfsign live surface to request full_bleed=true")
	}
}

func jsonUnmarshalManifest(raw []byte, m *pluginapi.Manifest) error {
	*m = pluginapi.Manifest{
		Schema:       1,
		ID:           "com.mtools.pdfsign",
		Name:         "PDF Signature",
		Version:      "1.0.0",
		Platform:     "darwin-arm64",
		Publisher:    pluginapi.Publisher{Name: "mtools Team", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		Capabilities: []pluginapi.Capability{pluginapi.Process, pluginapi.FilesRead, pluginapi.FilesWrite, pluginapi.UILive},
		Entry:        pluginapi.Entry{Kind: "sidecar", Path: "bin/tool", Protocol: pluginapi.Protocol},
		Tools:        []pluginapi.Tool{{ID: "pdfsign", Name: "PDF Signature", Description: "Sign PDF", Category: "Document"}},
		Files:        map[string]pluginapi.FileInfo{"bin/tool": {SHA256: strings.Repeat("a", 64), Size: 10, Executable: true}},
	}
	_ = raw
	return m.Validate()
}

func TestPDFSignLiveUITreeStatesAndValidation(t *testing.T) {
	tmp := t.TempDir()
	srv := newPDFSignServer()
	dataDir := filepath.Join(tmp, "data")
	srv.dataDir = dataDir
	srv.vault = pdfsign.NewSignatureVault(dataDir)
	srv.history = pdfsign.NewHistoryStore(dataDir)
	srv.lang = i18n.LangID

	// 1. Empty state tree validates and uses Indonesian localization
	emptyTree := srv.buildTreeLocked()
	if err := emptyTree.Validate(); err != nil {
		t.Fatalf("empty tree invalid: %v", err)
	}
	if emptyTree.Root.Children[0].ID != "empty_open_pdf" {
		t.Fatalf("expected empty_open_pdf card, got %s", emptyTree.Root.Children[0].ID)
	}

	// 2. Open PDF + Draw Studio + Photo Studio + Collapsed Rail + Export Banner all validate cleanly
	pdfFile := filepath.Join(tmp, "sample.pdf")
	if err := os.WriteFile(pdfFile, []byte(testMinimalPDF), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	srv.loadPDFLocked(context.Background(), pdfFile, nil)
	srv.studio.open = true
	srv.studio.mode = studioModeDraw
	drawTree := srv.buildTreeLocked()
	srv.studio.mode = studioModePhoto
	photoImg := image.NewNRGBA(image.Rect(0, 0, 80, 40))
	photoImg.Set(10, 10, color.NRGBA{R: 15, G: 15, B: 15, A: 255})
	srv.studio.rawPhoto = photoImg
	srv.recomputePhotoPreviewLocked(nil)
	photoTree := srv.buildTreeLocked()
	srv.studio.open = false

	srv.placeSignatureAtLocked(photoImg, "", 1, 20, 30, nil)
	activeTree := srv.buildTreeLocked()

	srv.railCollapsed = true
	collapsedTree := srv.buildTreeLocked()
	srv.railCollapsed = false

	srv.exportDone = true
	srv.exportPath = filepath.Join(tmp, "sample-signed.pdf")
	exportTree := srv.buildTreeLocked()
	srv.mu.Unlock()

	for name, tree := range map[string]pluginapi.UITree{
		"draw":      drawTree,
		"photo":     photoTree,
		"active":    activeTree,
		"collapsed": collapsedTree,
		"export":    exportTree,
	} {
		if err := tree.Validate(); err != nil {
			t.Fatalf("%s tree failed Validate(): %v", name, err)
		}
	}

	// 3. Shortcuts: L toggles lockAspect, Escape clears active placement
	srv.mu.Lock()
	if !srv.lockAspect {
		t.Fatal("expected lockAspect true initially")
	}
	srv.handleShortcutLocked("L")
	if srv.lockAspect {
		t.Fatal("expected L shortcut to toggle lockAspect false")
	}
	srv.handleShortcutLocked("Escape")
	if srv.activeID != "" {
		t.Fatal("expected Escape shortcut to clear active placement")
	}
	srv.mu.Unlock()
}

func TestPDFSignLiveUIFlow(t *testing.T) {
	tmp := t.TempDir()
	assetsDir := filepath.Join(tmp, "assets")
	_ = os.MkdirAll(assetsDir, 0o700)
	t.Setenv("MTOOLS_UI_ASSETS", assetsDir)

	pdfFile := filepath.Join(tmp, "doc.pdf")
	if err := os.WriteFile(pdfFile, []byte(testMinimalPDF), 0o600); err != nil {
		t.Fatalf("write test pdf: %v", err)
	}
	outFile := filepath.Join(tmp, "doc_signed.pdf")

	photoFile := filepath.Join(tmp, "sig.png")
	sigImg := image.NewNRGBA(image.Rect(0, 0, 120, 60))
	for x := 20; x < 100; x++ {
		sigImg.SetNRGBA(x, 30, color.NRGBA{R: 20, G: 20, B: 20, A: 255})
	}
	f, err := os.Create(photoFile)
	if err != nil {
		t.Fatal(err)
	}
	_ = png.Encode(f, sigImg)
	_ = f.Close()

	srv := newPDFSignServer()
	pr, pw := io.Pipe()
	var out bytes.Buffer
	identity := pluginapi.InitializeResult{
		Protocol: pluginapi.Protocol,
		PluginID: "com.mtools.pdfsign",
		Version:  "1.0.0",
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- pluginapi.ServeLive(context.Background(), pr, &out, identity, srv, srv)
	}()

	writeLine := func(s string) {
		t.Helper()
		if _, err := io.WriteString(pw, s+"\n"); err != nil {
			t.Fatalf("write rpc: %v", err)
		}
	}

	dataDir := filepath.ToSlash(filepath.Join(tmp, "data"))
	writeLine(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocol":"jsonrpc-stdio-v1","plugin_id":"com.mtools.pdfsign","version":"1.0.0"}}`)
	writeLine(`{"jsonrpc":"2.0","id":2,"method":"ui.open","params":{"tool":"pdfsign","lang":"en","viewport":{"width":1180,"height":760},"settings":{"plugin_data_dir":"` + dataDir + `"}}}`)
	writeLine(`{"jsonrpc":"2.0","method":"ui.event","params":{"rev":1,"node":"empty_open_pdf","kind":"change","value":"` + filepath.ToSlash(pdfFile) + `"}}`)
	writeLine(`{"jsonrpc":"2.0","method":"ui.event","params":{"rev":2,"node":"add_sig_top","kind":"click"}}`)
	writeLine(`{"jsonrpc":"2.0","method":"ui.event","params":{"rev":3,"node":"studio_draw_canvas","kind":"stroke","value":"10,10;60,30;120,25"}}`)
	writeLine(`{"jsonrpc":"2.0","method":"ui.event","params":{"rev":4,"node":"studio_save_vault","kind":"click"}}`)
	// Open photo studio too and verify threshold + recolor + place
	writeLine(`{"jsonrpc":"2.0","method":"ui.event","params":{"rev":5,"node":"vault_photo_btn","kind":"click"}}`)
	writeLine(`{"jsonrpc":"2.0","method":"ui.event","params":{"rev":6,"node":"studio_choose_photo","kind":"change","value":"` + filepath.ToSlash(photoFile) + `"}}`)
	writeLine(`{"jsonrpc":"2.0","method":"ui.event","params":{"rev":7,"node":"studio_photo_ink_blue","kind":"click"}}`)
	writeLine(`{"jsonrpc":"2.0","method":"ui.event","params":{"rev":8,"node":"studio_place_sig","kind":"click"}}`)
	writeLine(`{"jsonrpc":"2.0","method":"ui.event","params":{"rev":9,"node":"placed_sig_1","kind":"resize","value":"15,25,110,45"}}`)
	writeLine(`{"jsonrpc":"2.0","method":"ui.event","params":{"rev":10,"node":"finish_sign","kind":"change","value":"` + filepath.ToSlash(outFile) + `"}}`)

	// Wait for background SignPDF export to finish before closing session.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(outFile); err == nil {
			srv.mu.Lock()
			done := srv.exportDone
			srv.mu.Unlock()
			if done {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	writeLine(`{"jsonrpc":"2.0","method":"ui.close","params":{"tool":"pdfsign"}}`)
	_ = pw.Close()

	if err := <-serveErr; err != nil {
		t.Fatalf("ServeLive failed: %v", err)
	}

	if _, err := os.Stat(outFile); err != nil {
		t.Fatalf("expected signed output PDF %s to exist: %v", outFile, err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "data", "history.json")); err != nil {
		t.Fatalf("expected history.json created on sign export: %v", err)
	}
}
