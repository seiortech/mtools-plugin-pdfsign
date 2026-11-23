package pdfsign

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	SharedPDFiumWASMVersion = "v1.21.4"
	SharedPDFiumWASMURL     = "https://raw.githubusercontent.com/klippa-app/go-pdfium/" + SharedPDFiumWASMVersion + "/internal/pdfium_wasm/pdfium.wasm"
	SharedPDFiumWASMSHA256  = "e6f77e01dce34de05ad62515114152da580b64c23871e3e3e2afef3f7bb88ade"
	maxPDFiumWASMBytes      = 32 << 20 // 32 MiB hard cap
)

var (
	ErrPDFiumDownloadChecksum = errors.New("pdfium.wasm checksum mismatch")
	ErrPDFiumDownloadInvalid  = errors.New("downloaded file is not a valid WebAssembly module")
)

var wasmMagicHeader = []byte{0x00, 'a', 's', 'm'}

// DownloadPDFiumWASM downloads and verifies pdfium.wasm into targetPath atomically.
func DownloadPDFiumWASM(ctx context.Context, targetPath string, progress func(done float64, msg string)) error {
	return DownloadPDFiumWASMWithClient(ctx, &http.Client{Timeout: 5 * time.Minute}, SharedPDFiumWASMURL, SharedPDFiumWASMSHA256, targetPath, progress)
}

// DownloadPDFiumWASMWithClient downloads pdfium.wasm using the supplied HTTP client,
// verifying the WASM magic header and expected SHA-256 checksum before renaming into place.
func DownloadPDFiumWASMWithClient(ctx context.Context, client *http.Client, rawURL, expectedSHA256, targetPath string, progress func(done float64, msg string)) error {
	if targetPath == "" {
		return errors.New("target path is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create pdfium directory: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download pdfium.wasm: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download pdfium.wasm: HTTP %d", resp.StatusCode)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(targetPath), ".pdfium-*.wasm.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	limited := io.LimitReader(resp.Body, maxPDFiumWASMBytes+1)
	buf := make([]byte, 32*1024)
	var total int64
	var header [4]byte
	headerRead := 0
	lastReport := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			_ = tmpFile.Close()
			return err
		}
		n, readErr := limited.Read(buf)
		if n > 0 {
			if total+int64(n) > maxPDFiumWASMBytes {
				_ = tmpFile.Close()
				return fmt.Errorf("pdfium.wasm exceeds %d MiB limit", maxPDFiumWASMBytes>>20)
			}
			if headerRead < len(header) {
				copied := copy(header[headerRead:], buf[:n])
				headerRead += copied
			}
			if _, wErr := tmpFile.Write(buf[:n]); wErr != nil {
				_ = tmpFile.Close()
				return fmt.Errorf("write temp pdfium.wasm: %w", wErr)
			}
			_, _ = hasher.Write(buf[:n])
			total += int64(n)
			if progress != nil && time.Since(lastReport) >= 120*time.Millisecond {
				done := 0.0
				msg := fmt.Sprintf("Downloading pdfium.wasm (%d KB)...", total/1024)
				if resp.ContentLength > 0 {
					done = float64(total) / float64(resp.ContentLength)
					if done > 0.99 {
						done = 0.99
					}
					msg = fmt.Sprintf("Downloading pdfium.wasm (%.0f%%)...", done*100)
				}
				progress(done, msg)
				lastReport = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = tmpFile.Close()
			return fmt.Errorf("read pdfium.wasm stream: %w", readErr)
		}
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp pdfium.wasm: %w", err)
	}

	if headerRead < len(wasmMagicHeader) || !bytes.Equal(header[:], wasmMagicHeader) {
		return ErrPDFiumDownloadInvalid
	}

	gotSHA := hex.EncodeToString(hasher.Sum(nil))
	if expectedSHA256 != "" && gotSHA != expectedSHA256 {
		return fmt.Errorf("%w: got %s, want %s", ErrPDFiumDownloadChecksum, gotSHA, expectedSHA256)
	}

	_ = os.Remove(targetPath)
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("install pdfium.wasm: %w", err)
	}
	cleanupTmp = false
	if progress != nil {
		progress(1.0, "pdfium.wasm verified and ready")
	}
	return nil
}
