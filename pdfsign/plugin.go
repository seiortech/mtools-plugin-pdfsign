package pdfsign

import (
	"context"
	"embed"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/seiortech/mtools-sdk/ui/i18n"
	"github.com/seiortech/mtools-sdk/job"
	plugin "github.com/seiortech/mtools-sdk/settings"
	"github.com/seiortech/mtools-sdk/ui/tool"
)

//go:embed locales/*.json
var localesFS embed.FS

const (
	PluginID tool.PluginID = "com.mtools.pdfsign"
	ToolID   tool.ToolID   = "pdfsign"
)

// PluginSettingsSchema returns declared settings schema for PDF Signature.
func PluginSettingsSchema() []plugin.SettingField {
	return []plugin.SettingField{
		{
			Key:         "default_output_dir",
			Type:        plugin.SettingDirectory,
			Label:       "Default Output Folder",
			Description: "Destination folder for signed PDF files.",
			Default:     "~/Downloads",
			Required:    true,
		},
	}
}

// Plugin implements the native PDF Signature tool.
type Plugin struct {
	tool.PluginPaths
	store          plugin.SettingsStore
	queue          *job.Queue
	state          *ViewState
	vault          *SignatureVault
	history        *HistoryStore
	renderer       PageRenderer
	pageCache      *PageBitmapCache
	catalog        *i18n.Catalog
	langResolver   func() i18n.Lang
	externalRunner func(j SignJob) (job.Job, bool)
	redrawMu        sync.Mutex
	redrawFn        func()
	renderMu        sync.Mutex
	inFlightRenders map[PageCacheKey]bool
	renderSem       chan struct{}
	zoomGen         uint64
	zoomDebounce    *time.Timer
	vaultBitmaps    map[string]*ui.Bitmap
}

// LocaleTranslator implements tool.LocaleProvider.
func (p *Plugin) LocaleTranslator() i18n.Translator {
	return p.catalog
}

// SetLanguageResolver implements tool.LocaleProvider.
func (p *Plugin) SetLanguageResolver(fn func() i18n.Lang) {
	p.langResolver = fn
}

func (p *Plugin) effectiveLang() i18n.Lang {
	if p.store != nil {
		if raw := p.store.GetString("language"); raw != "" && raw != "global" {
			return i18n.NormalizeLang(raw)
		}
	}
	if p.langResolver != nil {
		return p.langResolver()
	}
	return i18n.CurrentLang()
}

func (p *Plugin) t(key string) string {
	if p.catalog == nil {
		return key
	}
	return p.catalog.T(p.effectiveLang(), key)
}

// NewPlugin creates a PDF Signature plugin instance.
func NewPlugin(dataDir string, store plugin.SettingsStore, queue *job.Queue) *Plugin {
	paths := tool.NewPluginPaths(dataDir)
	if store == nil {
		store, _ = plugin.NewJSONSettingsStore(paths.GetSettingsPath(), PluginSettingsSchema())
	}
	cat, err := i18n.NewCatalogFromFS(localesFS, "locales")
	if err != nil {
		cat = i18n.NewCatalog()
	}
	if dataDir != "" {
		_ = cat.LoadDir(filepath.Join(dataDir, "i18n"))
	}
	p := &Plugin{
		PluginPaths:     paths,
		store:           store,
		queue:           queue,
		state:           NewViewState(),
		vault:           NewSignatureVault(dataDir),
		history:         NewHistoryStore(dataDir),
		renderer:        DefaultRenderer(),
		pageCache:       NewPageBitmapCache(48),
		catalog:         cat,
		inFlightRenders: make(map[PageCacheKey]bool),
		renderSem:       make(chan struct{}, 2),
		vaultBitmaps:    make(map[string]*ui.Bitmap),
	}
	if queue != nil {
		queue.Subscribe(func() {
			p.requestRedraw()
		})
	}
	return p
}

// Vault returns the signature vault for persistent local signatures.
func (p *Plugin) Vault() *SignatureVault {
	if p.vault == nil {
		p.vault = NewSignatureVault(p.GetDefaultPath())
	}
	return p.vault
}

// Renderer returns the PageRenderer instance.
func (p *Plugin) Renderer() PageRenderer {
	if p.renderer == nil {
		p.renderer = DefaultRenderer()
	}
	return p.renderer
}

// SetRenderer overrides the renderer (useful for testing).
func (p *Plugin) SetRenderer(r PageRenderer) {
	p.renderer = r
}

// SetExternalRunner configures a runner that delegates SignJobs to an external sidecar bundle.
func (p *Plugin) SetExternalRunner(runner func(j SignJob) (job.Job, bool)) {
	p.externalRunner = runner
}

func (p *Plugin) enqueueJob(title string, j SignJob) job.JobID {
	if p.queue == nil {
		return ""
	}
	if p.externalRunner != nil {
		if extJob, ok := p.externalRunner(j); ok && extJob != nil {
			return p.queue.Enqueue(title, extJob)
		}
	}
	return p.queue.Enqueue(title, j)
}

// History returns the plugin history store.
func (p *Plugin) History() *HistoryStore {
	if p.history == nil {
		p.history = NewHistoryStore(p.GetDefaultPath())
	}
	return p.history
}

// HistoryEntries implements tool.HistoryProvider for global history.
func (p *Plugin) HistoryEntries() []tool.HistoryEntry {
	items := p.History().All()
	entries := make([]tool.HistoryEntry, len(items))
	for i, it := range items {
		subtitle := it.CompletedAt.Format("Jan 02, 15:04")
		if it.SignatureCount > 0 {
			subtitle = fmt.Sprintf("%d signature(s) · %s", it.SignatureCount, subtitle)
		}
		entries[i] = tool.HistoryEntry{
			ID:        it.ID,
			ToolID:    ToolID,
			ToolName:  p.t("pdfsign.name"),
			Title:     it.Summary,
			Subtitle:  subtitle,
			FilePath:  it.OutputFile,
			Timestamp: it.CompletedAt,
		}
	}
	return entries
}

// DeleteHistory removes an item by ID from the history store.
func (p *Plugin) DeleteHistory(id string) error {
	return p.History().Delete(id)
}

// ClearHistory removes all items from the history store.
func (p *Plugin) ClearHistory() error {
	return p.History().ClearAll()
}

// Meta returns tool metadata.
func (p *Plugin) Meta() tool.ToolMeta {
	return tool.ToolMeta{
		ID:          ToolID,
		PluginID:    PluginID,
		Name:        p.t("pdfsign.name"),
		Category:    "Document",
		Description: p.t("pdfsign.desc"),
		Icon:        "pen-tool",
	}
}

// Settings returns the settings store.
func (p *Plugin) Settings() plugin.SettingsStore {
	return p.store
}

// RunAction executes plugin-level action buttons.
func (p *Plugin) RunAction(ctx context.Context, actionID string) error {
	return fmt.Errorf("unknown action: %s", actionID)
}

// RevealFile reveals a file in the system file manager.
func RevealFile(filePath string) {
	mygo.Shell.ShowItemInFolder(filePath)
}

// OpenFile opens a file with default system handler.
func OpenFile(filePath string) error {
	return mygo.Shell.OpenPath(filePath)
}

// View mounts the native workspace UI.
func (p *Plugin) View(c *ui.Context) {
	p.redrawMu.Lock()
	p.redrawFn = c.Invalidate
	p.redrawMu.Unlock()
	p.RenderWorkspace(c)
}

func (p *Plugin) requestRedraw() {
	p.redrawMu.Lock()
	fn := p.redrawFn
	p.redrawMu.Unlock()
	if fn != nil {
		fn()
	}
}

// OnOpen implements tool.WorkspaceLifecycle.
func (p *Plugin) OnOpen() {
	if p.state == nil {
		p.state = NewViewState()
	}
	if p.pageCache == nil {
		p.pageCache = NewPageBitmapCache(48)
	}
	if p.vaultBitmaps == nil {
		p.vaultBitmaps = make(map[string]*ui.Bitmap)
	}
	_ = p.EnsureDirs()
}

// HasActiveJob implements tool.WorkspaceLifecycle.
func (p *Plugin) HasActiveJob() bool {
	if p.queue == nil || p.state == nil || p.state.ActiveJobID == "" {
		return false
	}
	rec, ok := p.queue.GetRecord(p.state.ActiveJobID)
	return ok && (rec.Status == job.StatusQueued || rec.Status == job.StatusRunning)
}

// OnClose implements tool.WorkspaceLifecycle.
func (p *Plugin) OnClose() {
	if p.queue != nil && p.state != nil && p.state.ActiveJobID != "" {
		p.queue.Cancel(p.state.ActiveJobID)
	}
	p.state = NewViewState()
	p.pageCache = NewPageBitmapCache(48)
	p.vaultBitmaps = make(map[string]*ui.Bitmap)
	_ = p.CleanTemp()
}
