package pdfsign

import (
	"path/filepath"
	"time"

	"github.com/seiortech/mtools-sdk/job"
)

// HistoryItem records metadata of a completed PDF signing operation.
type HistoryItem struct {
	ID             string    `json:"id"`
	Summary        string    `json:"summary"`
	OutputFile     string    `json:"output_file"`
	SignatureCount int       `json:"signature_count"`
	PageCount      int       `json:"page_count"`
	CompletedAt    time.Time `json:"completed_at"`
}

// GetID implements job.HistoryRecord.
func (h HistoryItem) GetID() string {
	return h.ID
}

// HistoryStore persists job history to JSON.
type HistoryStore = job.HistoryStore[HistoryItem]

// NewHistoryStore initializes history store from plugin data directory.
func NewHistoryStore(dataDir string) *HistoryStore {
	return job.NewHistoryStore[HistoryItem](filepath.Join(dataDir, "history.json"))
}
