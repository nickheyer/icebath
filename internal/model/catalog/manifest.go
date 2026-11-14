package catalog

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	DefaultManifestURL      = "https://icebath.app/docs/api/model-catalog.json"
	manifestFallbackURL     = "https://raw.githubusercontent.com/nickheyer/icebath/main/website/static/api/model-catalog.json"
	manifestDefaultTTL      = 20.0
	manifestFetchTimeout    = 8 * time.Second
	manifestRetryDelay      = 300 * time.Second
	manifestSchemaVersion   = 1
	manifestKind            = "manifest"
	manifestOverrideKind    = "override"
	manifestMinimumInterval = 60 * time.Second
)

type manifestModel struct {
	id          string
	description string
	isDefault   bool
}

type manifest struct {
	raw       wire.Value
	providers map[string][]manifestModel
}

type backoffKey struct {
	kind string
	url  string
}

type manifestState struct {
	mu         sync.Mutex
	cache      *manifest
	cacheMTime time.Time
	retryAfter map[backoffKey]time.Time
	refreshing bool
}

type manifestConfig struct {
	enabled   bool
	url       string
	ttl       time.Duration
	overrides map[string]string
}

func (c *Catalog) manifestBackingOff(kind, rawURL string) bool {
	st := &c.st.manifest
	st.mu.Lock()
	defer st.mu.Unlock()
	return time.Now().Before(st.retryAfter[backoffKey{kind, rawURL}])
}

func (c *Catalog) noteManifestFetch(kind, rawURL string, ok bool) {
	st := &c.st.manifest
	st.mu.Lock()
	defer st.mu.Unlock()
	if ok {
		delete(st.retryAfter, backoffKey{kind, rawURL})
		return
	}
	st.retryAfter[backoffKey{kind, rawURL}] = time.Now().Add(manifestRetryDelay)
}

func (c *Catalog) clearManifestBackoff() {
	st := &c.st.manifest
	st.mu.Lock()
	clear(st.retryAfter)
	st.mu.Unlock()
}

func (c *Catalog) manifestConfig(ctx context.Context) manifestConfig {
	out := manifestConfig{enabled: true, url: DefaultManifestURL, overrides: map[string]string{}}
	minutes := manifestDefaultTTL
	if cfg := c.config(ctx); cfg != nil {
		section := cfg.ModelCatalog
		out.enabled = section.Enabled
		if section.URL != "" {
			out.url = section.URL
		}
		minutes = section.TTLMinutes
		if minutes == manifestDefaultTTL && section.TTLHours != nil && *section.TTLHours != 0 {
			minutes = *section.TTLHours * 60.0
		}
		for _, name := range section.Providers.Keys() {
			if entry, ok := section.Providers.Get(name); ok {
				if override := strings.TrimSpace(entry.URL); override != "" {
					out.overrides[name] = override
				}
			}
		}
	}
	if minutes <= 0 {
		minutes = manifestDefaultTTL
	}
	out.ttl = time.Duration(minutes * float64(time.Minute))
	return out
}

func parseManifest(data []byte) *manifest {
	root, err := wire.ParseValue(data)
	if err != nil || !root.IsObject() {
		return nil
	}
	version, ok := jsonInt(root.Get("version"))
	if !ok || version > manifestSchemaVersion {
		return nil
	}
	blocks := root.Get("providers")
	if !blocks.IsObject() {
		return nil
	}
	out := &manifest{raw: root, providers: map[string][]manifestModel{}}
	for _, field := range blocks.Fields() {
		if !field.Value.IsObject() {
			return nil
		}
		models := field.Value.Get("models")
		if !models.IsArray() {
			return nil
		}
		rows := []manifestModel{}
		for _, model := range models.Elems() {
			id, isText := model.Get("id").Str()
			if !model.IsObject() || !isText || strings.TrimSpace(id) == "" {
				return nil
			}
			description := ""
			if raw := model.Get("description"); raw.Truthy() {
				description = raw.Display()
			}
			rows = append(rows, manifestModel{id: strings.TrimSpace(id), description: description, isDefault: model.Get("default").Truthy()})
		}
		out.providers[field.Key] = rows
	}
	return out
}

func (c *Catalog) manifestPath() string { return home.ModelCatalog.In(c.home) }

func readManifestFile(path string) *manifest {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseManifest(withoutBOM(data))
}

func (c *Catalog) readManifestDisk() (*manifest, time.Time) {
	path := c.manifestPath()
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}
	}
	parsed := readManifestFile(path)
	if parsed == nil {
		return nil, time.Time{}
	}
	return parsed, info.ModTime()
}

func (c *Catalog) writeManifestDisk(ctx context.Context, m *manifest) {
	if err := fileutil.WriteJSON(ctx, c.manifestPath(), json.RawMessage(m.raw.Raw()), fileutil.JSONStyle{Indent: 2}); err != nil {
		slog.Info("model catalog cache write failed", "error", err)
	}
}

func (c *Catalog) fetchManifest(ctx context.Context, rawURL string, timeout time.Duration) *manifest {
	header := http.Header{"Accept": {"application/json"}, "User-Agent": {c.userAgent()}}
	resp, err := c.get(ctx, rawURL, header, "", timeoutPhases(timeout))
	if err == nil && !resp.success() {
		err = &statusError{status: resp.status}
	}
	if err != nil {
		slog.Info("model catalog fetch failed", "url", rawURL, "error", err)
		return nil
	}
	parsed := parseManifest(resp.body)
	if parsed == nil {
		slog.Info("model catalog failed schema validation", "url", rawURL)
	}
	return parsed
}

func (c *Catalog) fetchManifestWithFallback(ctx context.Context, primary string, timeout time.Duration) *manifest {
	if fetched := c.fetchManifest(ctx, primary, timeout); fetched != nil {
		return fetched
	}
	if manifestFallbackURL == primary {
		return nil
	}
	fetched := c.fetchManifest(ctx, manifestFallbackURL, timeout)
	if fetched != nil {
		slog.Info("model catalog primary URL failed, using fallback", "url", manifestFallbackURL)
	}
	return fetched
}

func (c *Catalog) spawnManifestRefresh(ctx context.Context, rawURL string) {
	st := &c.st.manifest
	st.mu.Lock()
	if st.refreshing {
		st.mu.Unlock()
		return
	}
	st.refreshing = true
	st.mu.Unlock()
	started := c.background(ctx, func(bg context.Context) {
		if fetched := c.fetchManifestWithFallback(bg, rawURL, manifestFetchTimeout); fetched != nil {
			c.writeManifestDisk(bg, fetched)
		}
		st.mu.Lock()
		st.refreshing = false
		st.mu.Unlock()
	})
	if !started {
		st.mu.Lock()
		st.refreshing = false
		st.mu.Unlock()
	}
}

func (c *Catalog) rememberManifest(m *manifest, mtime time.Time) *manifest {
	st := &c.st.manifest
	st.mu.Lock()
	st.cache, st.cacheMTime = m, mtime
	st.mu.Unlock()
	return m
}

func (c *Catalog) inProcessManifest() (*manifest, time.Time) {
	st := &c.st.manifest
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cache, st.cacheMTime
}

func (c *Catalog) manifest(ctx context.Context, force bool) *manifest {
	cfg := c.manifestConfig(ctx)
	if !cfg.enabled {
		return nil
	}
	disk, diskMTime := c.readManifestDisk()
	now := time.Now()
	diskFresh := disk != nil && now.Sub(diskMTime) < cfg.ttl
	if !force && disk != nil {
		cached, cachedMTime := c.inProcessManifest()
		if diskFresh && cached != nil && diskMTime.Equal(cachedMTime) {
			return cached
		}
		if !diskFresh {
			c.spawnManifestRefresh(ctx, cfg.url)
		}
		return c.rememberManifest(disk, diskMTime)
	}
	if !force && disk == nil && c.manifestBackingOff(manifestKind, cfg.url) {
		return nil
	}
	fetched := c.fetchManifestWithFallback(ctx, cfg.url, manifestFetchTimeout)
	c.noteManifestFetch(manifestKind, cfg.url, fetched != nil)
	if fetched != nil {
		c.writeManifestDisk(ctx, fetched)
		if written, writtenMTime := c.readManifestDisk(); written != nil {
			return c.rememberManifest(written, writtenMTime)
		}
		return c.rememberManifest(fetched, now)
	}
	if disk != nil {
		return c.rememberManifest(disk, diskMTime)
	}
	return nil
}

func (c *Catalog) ManifestRefreshInterval(ctx context.Context) time.Duration {
	return max(manifestMinimumInterval, c.manifestConfig(c.scoped(ctx)).ttl)
}

func (c *Catalog) portalSignedIn(ctx context.Context) bool {
	if state, ok := c.portal.State(ctx); ok && len(state) > 0 && !portal.IsGuestState(state) {
		return true
	}
	if pool, err := c.auth.Pool(ctx, providers.ProviderPortal); err == nil {
		for _, entry := range pool.Entries() {
			if encoded, err := entry.MarshalJSON(); err == nil && !portal.IsGuestState(encoded) {
				return true
			}
		}
	}
	return c.env(ctx, "ICEBATH_PORTAL_API_KEY") != ""
}

func (c *Catalog) RefreshCatalogs(ctx context.Context) bool {
	ctx = c.scoped(ctx)
	if !c.manifestConfig(ctx).enabled {
		return false
	}
	c.clearManifestBackoff()
	refreshed := c.manifest(ctx, true)
	c.fetchOpenRouterModels(ctx, openRouterListTimeout, true, false)
	if c.portalSignedIn(ctx) {
		c.fetchPortalRecommendedModels(ctx, c.resolvePortalURL(ctx), recommendedTimeout, true)
	}
	return refreshed != nil
}

func (c *Catalog) providerOverrideManifest(ctx context.Context, provider string) *manifest {
	cfg := c.manifestConfig(ctx)
	if !cfg.enabled {
		return nil
	}
	override := cfg.overrides[provider]
	if override == "" || c.manifestBackingOff(manifestOverrideKind, override) {
		return nil
	}
	fetched := c.fetchManifest(ctx, override, manifestFetchTimeout)
	c.noteManifestFetch(manifestOverrideKind, override, fetched != nil)
	return fetched
}

func manifestBlock(m *manifest, provider string) ([]manifestModel, bool) {
	if m == nil {
		return nil, false
	}
	rows, ok := m.providers[provider]
	return rows, ok
}

func (c *Catalog) manifestProviderBlock(ctx context.Context, provider string) []manifestModel {
	if rows, ok := manifestBlock(c.providerOverrideManifest(ctx, provider), provider); ok {
		return rows
	}
	rows, _ := manifestBlock(c.manifest(ctx, false), provider)
	return rows
}

func (c *Catalog) curatedOpenRouterModels(ctx context.Context) []ModelChoice {
	var out []ModelChoice
	for _, row := range c.manifestProviderBlock(ctx, "openrouter") {
		out = append(out, ModelChoice{ID: row.id, Description: row.description})
	}
	return out
}

func (c *Catalog) curatedPortalModels(ctx context.Context) []string {
	var out []string
	for _, row := range c.manifestProviderBlock(ctx, providers.ProviderPortal) {
		out = append(out, row.id)
	}
	return out
}

func defaultModelOf(rows []manifestModel) string {
	for _, row := range rows {
		if row.isDefault {
			return row.id
		}
	}
	return ""
}

func (c *Catalog) defaultModelFromCache(provider string) string {
	if cached, _ := c.inProcessManifest(); cached != nil {
		if found := defaultModelOf(cached.providers[provider]); found != "" {
			return found
		}
	}
	disk, _ := c.readManifestDisk()
	if disk == nil {
		return ""
	}
	return defaultModelOf(disk.providers[provider])
}

func (c *Catalog) ResetManifestCache() {
	c.clearManifestBackoff()
	st := &c.st.manifest
	st.mu.Lock()
	st.cache, st.cacheMTime = nil, time.Time{}
	st.mu.Unlock()
}

func (c *Catalog) SeedManifestCache(ctx context.Context, source string) bool {
	ctx = c.scoped(ctx)
	seed := readManifestFile(source)
	if seed == nil {
		slog.Debug("model catalog seed skipped, the file is unreadable or not a valid manifest", "source", source)
		return false
	}
	c.writeManifestDisk(ctx, seed)
	c.ResetManifestCache()
	return true
}
