package pdfsign

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const MaxSavedSignatures = 10

var (
	ErrVaultFull         = errors.New("maximum of 10 saved signatures reached")
	ErrSignatureNotFound = errors.New("signature not found")
	ErrEmptySignature    = errors.New("empty signature image data")
)

// SignatureItem represents metadata for one saved signature in the local vault.
type SignatureItem struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	SourceKind string    `json:"source_kind"` // "drawn" or "photo"
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	CreatedAt  time.Time `json:"created_at"`
}

// SignatureVault manages persistent local storage of up to 10 signatures.
type SignatureVault struct {
	mu      sync.RWMutex
	dir     string
	items   []SignatureItem
	fileMap map[string]string // id -> png path
	nextSeq uint64
}

func NewSignatureVault(dataDir string) *SignatureVault {
	sigDir := filepath.Join(dataDir, "signatures")
	_ = os.MkdirAll(sigDir, 0700)
	v := &SignatureVault{
		dir:     sigDir,
		fileMap: make(map[string]string),
	}
	_ = v.loadIndex()
	return v
}

func (v *SignatureVault) indexPath() string {
	return filepath.Join(v.dir, "index.json")
}

func (v *SignatureVault) signaturePath(id string) string {
	return filepath.Join(v.dir, id+".png")
}

func (v *SignatureVault) loadIndex() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	data, err := os.ReadFile(v.indexPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			v.items = []SignatureItem{}
			return nil
		}
		return err
	}

	var items []SignatureItem
	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}

	valid := make([]SignatureItem, 0, len(items))
	v.fileMap = make(map[string]string)
	for _, it := range items {
		p := v.signaturePath(it.ID)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			valid = append(valid, it)
			v.fileMap[it.ID] = p
		}
	}
	v.items = valid
	return nil
}

func (v *SignatureVault) saveIndexLocked() error {
	data, err := json.MarshalIndent(v.items, "", "  ")
	if err != nil {
		return err
	}
	tmpFile := filepath.Join(v.dir, "index.tmp.json")
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmpFile, v.indexPath())
}

// List returns a snapshot of all saved signatures, newest first.
func (v *SignatureVault) List() []SignatureItem {
	v.mu.RLock()
	defer v.mu.RUnlock()
	res := make([]SignatureItem, len(v.items))
	copy(res, v.items)
	return res
}

// Count returns the current number of saved signatures.
func (v *SignatureVault) Count() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.items)
}

// Get returns the PNG bytes and metadata for a specific signature ID.
func (v *SignatureVault) Get(id string) (SignatureItem, []byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	for _, it := range v.items {
		if it.ID == id {
			p, ok := v.fileMap[id]
			if !ok {
				p = v.signaturePath(id)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return SignatureItem{}, nil, err
			}
			return it, data, nil
		}
	}
	return SignatureItem{}, nil, ErrSignatureNotFound
}

// Save stores a new PNG signature into the vault. Enforces the max 10 limit.
func (v *SignatureVault) Save(name string, sourceKind string, width, height int, pngBytes []byte) (SignatureItem, error) {
	if len(pngBytes) == 0 {
		return SignatureItem{}, ErrEmptySignature
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if len(v.items) >= MaxSavedSignatures {
		return SignatureItem{}, ErrVaultFull
	}

	v.nextSeq++
	id := fmt.Sprintf("sig_%d_%d", time.Now().UnixNano(), v.nextSeq)
	if name == "" {
		name = fmt.Sprintf("Signature %d", len(v.items)+1)
	}
	if sourceKind == "" {
		sourceKind = "drawn"
	}

	item := SignatureItem{
		ID:         id,
		Name:       name,
		SourceKind: sourceKind,
		Width:      width,
		Height:     height,
		CreatedAt:  time.Now(),
	}

	tmpPNG := filepath.Join(v.dir, fmt.Sprintf(".tmp_%s.png", id))
	if err := os.WriteFile(tmpPNG, pngBytes, 0600); err != nil {
		return SignatureItem{}, err
	}

	finalPNG := v.signaturePath(id)
	if err := os.Rename(tmpPNG, finalPNG); err != nil {
		_ = os.Remove(tmpPNG)
		return SignatureItem{}, err
	}

	// Insert newest first
	v.items = append([]SignatureItem{item}, v.items...)
	v.fileMap[id] = finalPNG

	if err := v.saveIndexLocked(); err != nil {
		_ = os.Remove(finalPNG)
		v.items = v.items[1:]
		delete(v.fileMap, id)
		return SignatureItem{}, err
	}

	return item, nil
}

// SaveImage encodes and saves an image.Image into the vault.
func (v *SignatureVault) SaveImage(name string, sourceKind string, img image.Image) (SignatureItem, error) {
	if img == nil {
		return SignatureItem{}, ErrEmptySignature
	}
	bytes, err := EncodePNGToBytes(img)
	if err != nil {
		return SignatureItem{}, err
	}
	bounds := img.Bounds()
	return v.Save(name, sourceKind, bounds.Dx(), bounds.Dy(), bytes)
}

// LoadImage loads and decodes the image.Image for a given signature ID.
func (v *SignatureVault) LoadImage(id string) (image.Image, error) {
	_, data, err := v.Get(id)
	if err != nil {
		return nil, err
	}
	return png.Decode(bytes.NewReader(data))
}

// Delete removes a signature by ID from the vault.
func (v *SignatureVault) Delete(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	idx := -1
	for i, it := range v.items {
		if it.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ErrSignatureNotFound
	}

	v.items = append(v.items[:idx], v.items[idx+1:]...)
	pngPath := v.fileMap[id]
	if pngPath == "" {
		pngPath = v.signaturePath(id)
	}
	delete(v.fileMap, id)
	_ = os.Remove(pngPath)

	return v.saveIndexLocked()
}
