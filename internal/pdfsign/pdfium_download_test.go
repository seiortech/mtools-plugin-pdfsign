package pdfsign

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestDownloadPDFiumWASMSuccessAndBadChecksum(t *testing.T) {
	validPayload := append([]byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00}, []byte("test-wasm-body")...)
	sum := sha256.Sum256(validPayload)
	validHash := hex.EncodeToString(sum[:])

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var body []byte
			status := http.StatusOK
			switch r.URL.Path {
			case "/valid.wasm":
				body = validPayload
			case "/not-wasm.bin":
				body = []byte("not a wasm binary")
			default:
				status = http.StatusNotFound
			}
			return &http.Response{
				StatusCode:    status,
				ContentLength: int64(len(body)),
				Body:          io.NopCloser(bytes.NewReader(body)),
				Header:        make(http.Header),
			}, nil
		}),
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "tools", "pdfium.wasm")

	if err := DownloadPDFiumWASMWithClient(context.Background(), client, "https://example.invalid/valid.wasm", validHash, target, nil); err != nil {
		t.Fatalf("expected valid download to succeed, got %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read installed file: %v", err)
	}
	if string(got) != string(validPayload) {
		t.Fatalf("installed payload mismatch")
	}

	badTarget := filepath.Join(dir, "tools", "bad.wasm")
	err = DownloadPDFiumWASMWithClient(context.Background(), client, "https://example.invalid/valid.wasm", "0000000000000000000000000000000000000000000000000000000000000000", badTarget, nil)
	if !errors.Is(err, ErrPDFiumDownloadChecksum) {
		t.Fatalf("expected ErrPDFiumDownloadChecksum, got %v", err)
	}
	if _, statErr := os.Stat(badTarget); !os.IsNotExist(statErr) {
		t.Fatalf("bad target should not exist after checksum failure")
	}

	invalidTarget := filepath.Join(dir, "tools", "invalid.wasm")
	err = DownloadPDFiumWASMWithClient(context.Background(), client, "https://example.invalid/not-wasm.bin", "", invalidTarget, nil)
	if !errors.Is(err, ErrPDFiumDownloadInvalid) {
		t.Fatalf("expected ErrPDFiumDownloadInvalid, got %v", err)
	}
}
