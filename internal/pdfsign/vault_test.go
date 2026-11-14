package pdfsign_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/seiortech/mtools-plugin-pdfsign/internal/pdfsign"
)

func createDummyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 31, G: 40, B: 34, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSignatureVaultMax10AndPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	v := pdfsign.NewSignatureVault(tmpDir)

	if cnt := v.Count(); cnt != 0 {
		t.Fatalf("expected 0 items, got %d", cnt)
	}

	dummy := createDummyPNG(t, 100, 50)

	// Save 10 signatures
	var savedIDs []string
	for i := 1; i <= 10; i++ {
		it, err := v.Save("Sig", "drawn", 100, 50, dummy)
		if err != nil {
			t.Fatalf("failed to save signature %d: %v", i, err)
		}
		savedIDs = append(savedIDs, it.ID)
	}

	if cnt := v.Count(); cnt != 10 {
		t.Fatalf("expected 10 items, got %d", cnt)
	}

	// 11th save must fail with ErrVaultFull
	_, err := v.Save("Sig 11", "drawn", 100, 50, dummy)
	if err != pdfsign.ErrVaultFull {
		t.Fatalf("expected ErrVaultFull on 11th save, got: %v", err)
	}

	// Retrieve one
	it, data, err := v.Get(savedIDs[0])
	if err != nil {
		t.Fatalf("failed to get signature: %v", err)
	}
	if it.ID != savedIDs[0] || len(data) == 0 {
		t.Fatalf("unexpected signature item data: %+v", it)
	}

	// Delete one
	if err := v.Delete(savedIDs[0]); err != nil {
		t.Fatalf("failed to delete signature: %v", err)
	}
	if cnt := v.Count(); cnt != 9 {
		t.Fatalf("expected 9 items after delete, got %d", cnt)
	}

	// Now can save again
	newIt, err := v.Save("New Slot", "photo", 120, 60, dummy)
	if err != nil {
		t.Fatalf("expected successful save in freed slot, got: %v", err)
	}
	if v.Count() != 10 {
		t.Fatalf("expected 10 items after save, got %d", v.Count())
	}

	// Test persistence across instances
	v2 := pdfsign.NewSignatureVault(tmpDir)
	if v2.Count() != 10 {
		t.Fatalf("expected reloaded vault to have 10 items, got %d", v2.Count())
	}
	_, data2, err := v2.Get(newIt.ID)
	if err != nil || len(data2) == 0 {
		t.Fatalf("failed to get signature from reloaded vault: %v", err)
	}
}
