// The provider catalog cache: the durable half of the live pick list.
//
// POST /agent/v1/config/peers.catalog (peercatalog.go) is the live probe
// — one provider, a 15s cap, a clean 502 when that provider is down.
// Probing every provider on page load would be slow and rude, so every
// successful probe also records its result here, and
// GET /agent/v1/config/peers.catalog serves those records instantly with
// no upstream calls at all. That is what hydrates the config screen's
// model pickers and the Providers screen on first paint.
//
// The cache is a plain JSON file next to the config (providers-cache.json,
// mode 0600, written temp-and-rename so a crash never leaves half a file).
// It holds model IDs and metadata only: never an API key, never a resolved
// env value, never a header. A provider that returns an absurd number of
// models is capped so one giant catalog cannot bloat the file.
package agentapi

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// providersCacheFileName is the cache file name inside the config
	// directory (or next to the -config file).
	providersCacheFileName = "providers-cache.json"

	// providersCacheVersion lets a future shape change be detected
	// instead of silently mis-parsed.
	providersCacheVersion = 1

	// maxCatalogModelsPerPeer caps one provider's recorded catalog. A
	// gateway can list tens of thousands of models; the pick list only
	// needs the head of the sorted list, and the file must stay small.
	maxCatalogModelsPerPeer = 500

	// maxCatalogPeers caps how many providers are kept, dropping the
	// least recently checked first.
	maxCatalogPeers = 64

	// providersCacheMode is 0600: the cache is the server's business,
	// not everybody's on the box.
	providersCacheMode os.FileMode = 0o600
)

// DefaultProvidersCachePath returns the cache file path for a config
// source. When source is a directory (config-dir mode) the file lands
// inside it; when it is a file (-config mode) it lands next to it. Mirrors
// engineupdate.DefaultStatePath so the two sidecar files always agree.
func DefaultProvidersCachePath(source string) string {
	if source == "" {
		return providersCacheFileName
	}
	info, err := os.Stat(source)
	if err == nil && info.IsDir() {
		return filepath.Join(source, providersCacheFileName)
	}
	return filepath.Join(filepath.Dir(source), providersCacheFileName)
}

// cachedCatalog is one provider's last successful catalog.
type cachedCatalog struct {
	// CheckedAt is when the successful probe finished.
	CheckedAt time.Time `json:"checkedAt"`

	// LatencyMs is how long that probe took.
	LatencyMs int64 `json:"latencyMs"`

	// Models are the model IDs the provider reported, sorted, capped.
	Models []peerCatalogModelDTO `json:"models"`

	// Truncated reports that the provider listed more than
	// maxCatalogModelsPerPeer models and only the first slice was kept.
	Truncated bool `json:"truncated,omitempty"`

	// TotalModels is how many the provider listed before the cap.
	TotalModels int `json:"totalModels,omitempty"`
}

// providersCacheFile is the on-disk shape.
type providersCacheFile struct {
	Version int                      `json:"version"`
	Peers   map[string]cachedCatalog `json:"peers"`
	SavedAt *time.Time               `json:"savedAt,omitempty"`
}

// catalogCache is the in-memory view of the file. All access goes through
// the mutex: probes finish on many goroutines at once.
type catalogCache struct {
	mu      sync.Mutex
	entries map[string]cachedCatalog
}

func newCatalogCache() *catalogCache {
	return &catalogCache{entries: make(map[string]cachedCatalog)}
}

// capEntry enforces the per-provider model cap, flagging the truncation
// so the UI can say "showing 500 of 12,345".
func capEntry(entry cachedCatalog) cachedCatalog {
	if len(entry.Models) <= maxCatalogModelsPerPeer {
		return entry
	}
	entry.TotalModels = len(entry.Models)
	entry.Models = entry.Models[:maxCatalogModelsPerPeer]
	entry.Truncated = true
	return entry
}

// loadCatalogCache reads path into a fresh cache. A missing file is an
// empty cache, not an error (first run); a corrupt file is an error and an
// empty cache, so the caller can log it and carry on — a broken cache must
// never stop the server from booting.
func loadCatalogCache(path string) (*catalogCache, error) {
	cache := newCatalogCache()
	if path == "" {
		return cache, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cache, nil
		}
		return cache, fmt.Errorf("reading providers cache: %w", err)
	}
	var file providersCacheFile
	if err := json.Unmarshal(data, &file); err != nil {
		return cache, fmt.Errorf("providers cache %s is corrupt: %w", path, err)
	}
	for name, entry := range file.Peers {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		cache.entries[name] = capEntry(entry)
	}
	return cache, nil
}

// snapshot returns a copy safe to read without the lock.
func (c *catalogCache) snapshot() map[string]cachedCatalog {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]cachedCatalog, len(c.entries))
	for name, entry := range c.entries {
		copied := entry
		copied.Models = append([]peerCatalogModelDTO(nil), entry.Models...)
		out[name] = copied
	}
	return out
}

// put records one provider's catalog: that provider's entry only, so a
// successful probe of one provider never disturbs any other.
func (c *catalogCache) put(name string, entry cachedCatalog) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[name] = capEntry(entry)
	if len(c.entries) <= maxCatalogPeers {
		return
	}
	// Over the provider cap: drop the least recently checked provider.
	names := make([]string, 0, len(c.entries))
	for n := range c.entries {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		return c.entries[names[i]].CheckedAt.Before(c.entries[names[j]].CheckedAt)
	})
	for _, n := range names[:len(c.entries)-maxCatalogPeers] {
		delete(c.entries, n)
	}
}

// save writes the cache atomically: a temp file in the same directory,
// chmod 0600, fsync-free rename over the target. A crash mid-write leaves
// either the old file or the new one, never a truncated mix.
func (c *catalogCache) save(path string) error {
	if path == "" {
		return fmt.Errorf("no providers cache path configured")
	}
	c.mu.Lock()
	file := providersCacheFile{
		Version: providersCacheVersion,
		Peers:   make(map[string]cachedCatalog, len(c.entries)),
	}
	now := time.Now().UTC()
	file.SavedAt = &now
	for name, entry := range c.entries {
		copied := entry
		copied.Models = append([]peerCatalogModelDTO(nil), entry.Models...)
		file.Peers[name] = copied
	}
	c.mu.Unlock()

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding providers cache: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating providers cache directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".providers-cache-*.json")
	if err != nil {
		return fmt.Errorf("creating temporary providers cache: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best effort: on the success path the rename already moved it.
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(providersCacheMode); err != nil {
		tmp.Close()
		return fmt.Errorf("setting providers cache permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing providers cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing providers cache: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing providers cache: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// api wiring
// ---------------------------------------------------------------------------

// providersCachePath is where the cache lives, per request so a hot reload
// or a later flag change is picked up. Falls back to the config file's
// directory when main never wired an explicit path (tests, embedded uses).
func (a *api) providersCachePath() string {
	if a.opts.ProvidersCachePath != nil {
		return a.opts.ProvidersCachePath()
	}
	if path := a.configPath(); path != "" {
		return DefaultProvidersCachePath(path)
	}
	return ""
}

// catalogs returns the lazily loaded cache. The first read loads the file
// so a restart keeps the last catalogs it knew about.
func (a *api) catalogs() *catalogCache {
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	if a.catalogLoaded {
		return a.catalog
	}
	cache, err := loadCatalogCache(a.providersCachePath())
	if err != nil {
		// A corrupt or unreadable cache is worth a log line, never a
		// failed request: the live probe still works.
		log.Printf("agent api: %v", err)
	}
	a.catalog = cache
	a.catalogLoaded = true
	return cache
}

// storeCatalog records a successful probe and persists the whole cache.
// Persistence failures are logged only: the probe itself succeeded, and a
// read-only config directory must not turn a working picker into an error.
func (a *api) storeCatalog(name string, models []peerCatalogModelDTO, latency time.Duration) {
	if name == "" {
		// An unsaved (draft) peer has nothing to key the entry on.
		return
	}
	cache := a.catalogs()
	cache.put(name, cachedCatalog{
		CheckedAt: time.Now().UTC(),
		LatencyMs: latency.Milliseconds(),
		Models:    models,
	})
	if err := cache.save(a.providersCachePath()); err != nil {
		log.Printf("agent api: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GET /agent/v1/config/peers.catalog
// ---------------------------------------------------------------------------

// peerCatalogCachePeerDTO is one provider row of the cached catalog: the
// configured facts the Providers screen shows plus whatever the last
// successful probe learned.
type peerCatalogCachePeerDTO struct {
	Name string `json:"name"`
	// Proxy is the configured proxy URL. It is a URL, never a secret.
	Proxy string `json:"proxy"`
	// KeyMacro is the ${env.VAR} NAME the config references for this
	// peer's apiKey, empty when the file holds no macro (a literal, or
	// nothing). The value itself is never read or returned.
	KeyMacro string `json:"keyMacro,omitempty"`
	// KeyMacroAvailable says whether that macro resolves in the server
	// environment right now — the most common silent provider death.
	// Absent when there is no macro to resolve.
	KeyMacroAvailable *bool `json:"keyMacroAvailable,omitempty"`

	// Models is the last successful catalog; empty when never checked.
	Models     []peerCatalogModelDTO `json:"models"`
	ModelCount int                   `json:"modelCount"`
	// CheckedAt is the RFC3339 UTC time of the last successful check.
	CheckedAt string `json:"checkedAt,omitempty"`
	LatencyMs int64  `json:"latencyMs,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// TotalModels is the provider's full listing size when truncated.
	TotalModels int `json:"totalModels,omitempty"`
}

// peerCatalogCacheResponse is the wire shape of the cached read.
type peerCatalogCacheResponse struct {
	Peers       []peerCatalogCachePeerDTO `json:"peers"`
	GeneratedAt string                    `json:"generatedAt"`
}

// handlePeerCatalogCache is GET /agent/v1/config/peers.catalog: every
// configured peer with its cached catalog. No upstream calls, so it is
// fast enough for page load.
func (a *api) handlePeerCatalogCache(w http.ResponseWriter, r *http.Request) {
	raw, err := a.readConfigFile()
	if err != nil {
		// The in-memory config is still a usable (macro-less) view when
		// the server runs from -config-dir only.
		raw = nil
	}
	macros := peerKeyMacros(raw)
	cached := a.catalogs().snapshot()

	names := make([]string, 0, len(a.cfg.Peers))
	for name := range a.cfg.Peers {
		names = append(names, name)
	}
	sort.Strings(names)

	resp := peerCatalogCacheResponse{
		Peers:       make([]peerCatalogCachePeerDTO, 0, len(names)),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	for _, name := range names {
		peer := a.cfg.Peers[name]
		dto := peerCatalogCachePeerDTO{
			Name:   name,
			Proxy:  peer.Proxy,
			Models: []peerCatalogModelDTO{},
			// The configured model list is what the peer declares; the
			// catalog is what it actually serves. Until a check has run,
			// count what the config says.
			ModelCount: len(peer.Models),
		}
		if macro := macros[name]; macro != "" {
			dto.KeyMacro = macro
			if m := envMacroRef.FindStringSubmatch(macro); m != nil {
				available := os.Getenv(m[1]) != ""
				dto.KeyMacroAvailable = &available
			}
		}
		if entry, ok := cached[name]; ok {
			dto.Models = entry.Models
			dto.ModelCount = len(entry.Models)
			if entry.CheckedAt.IsZero() == false {
				dto.CheckedAt = entry.CheckedAt.UTC().Format(time.RFC3339)
			}
			dto.LatencyMs = entry.LatencyMs
			dto.Truncated = entry.Truncated
			dto.TotalModels = entry.TotalModels
		}
		resp.Peers = append(resp.Peers, dto)
	}
	writeJSON(w, r, http.StatusOK, resp)
}

// peerKeyMacros maps a peer name to the ${env.VAR} macro the raw config
// file references for its apiKey. Peers whose apiKey is a literal (or
// absent) are simply missing from the map — the value is never read.
func peerKeyMacros(raw []byte) map[string]string {
	out := make(map[string]string)
	root := parseRawRoot(raw)
	peers := mappingValue(root, "peers")
	if peers == nil || peers.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(peers.Content); i += 2 {
		name := peers.Content[i].Value
		node := mappingValue(peers.Content[i+1], "apiKey")
		if node == nil || node.Kind != yaml.ScalarNode {
			continue
		}
		if envMacroRef.MatchString(node.Value) {
			out[name] = node.Value
		}
	}
	return out
}
