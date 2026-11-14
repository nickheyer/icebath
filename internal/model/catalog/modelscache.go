package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	providerModelsCacheTTL      = time.Hour
	providerModelsStaleServeMax = 7 * 24 * time.Hour
	providerModelsFallbackTTL   = 60 * time.Second
	providerModelsLockWait      = 10 * time.Second
	ollamaLocalModelsCacheTTL   = 300 * time.Second
)

type diskCacheState struct {
	mu         sync.Mutex
	refreshing map[string]bool
	write      sync.Mutex
}

type cacheRow struct {
	valid    bool
	fp       string
	at       float64
	models   []string
	fallback bool
	native   bool
}

type cacheEntry struct {
	Fingerprint string          `json:"fp"`
	At          json.RawMessage `json:"at"`
	Models      []string        `json:"models"`
	Fallback    bool            `json:"fallback,omitempty"`
	Native      *bool           `json:"native_catalog,omitempty"`
}

type APIModelsQuery struct {
	Key       auth.Credential
	BaseURL   string
	Timeout   time.Duration
	Mode      string
	Headers   map[string]string
	Force     bool
	CacheOnly bool
	TTL       time.Duration
	Fetch     func(ctx context.Context) (ids []string, native, listed bool)
}

func newCacheEntry(fp string, models []string, at float64) cacheEntry {
	if models == nil {
		models = []string{}
	}
	return cacheEntry{Fingerprint: fp, At: json.RawMessage(strconv.FormatFloat(at, 'f', -1, 64)), Models: slices.Clone(models)}
}

func parseCacheRow(raw json.RawMessage) cacheRow {
	entry, err := wire.ParseValue(raw)
	if err != nil || !entry.IsObject() {
		return cacheRow{}
	}
	row := cacheRow{}
	row.fp, _ = entry.Get("fp").Str()
	models := entry.Get("models")
	stamp := entry.Get("at")
	at, numeric := stamp.Float()
	if !models.IsArray() || stamp.Kind() != wire.KindNumber || !numeric {
		return cacheRow{}
	}
	row.valid, row.at = true, at
	row.models = make([]string, 0, len(models.Elems()))
	for _, model := range models.Elems() {
		row.models = append(row.models, model.Display())
	}
	row.fallback = entry.Get("fallback").Truthy()
	row.native = entry.Get("native_catalog").True()
	return row
}

func (r cacheRow) validFor(fp string, allowEmpty bool) bool {
	return r.valid && r.fp == fp && (allowEmpty || len(r.models) > 0)
}

func diskServeTier(row cacheRow, fp string, now float64, ollama bool, ttl time.Duration) string {
	if ollama {
		ttl = min(ttl, ollamaLocalModelsCacheTTL)
	}
	if !row.validFor(fp, ollama) {
		return ""
	}
	age := now - row.at
	limit := ttl
	if row.fallback {
		limit = providerModelsFallbackTTL
	}
	if age < limit.Seconds() {
		return "fresh"
	}
	if len(row.models) > 0 && !row.fallback && age < providerModelsStaleServeMax.Seconds() {
		return "stale"
	}
	return ""
}

func (c *Catalog) providerModelsCachePath() string { return home.ProviderModelsCache.In(c.home) }

func (c *Catalog) loadProviderModelsCache() map[string]json.RawMessage {
	return readJSONObject(c.providerModelsCachePath())
}

func (c *Catalog) writeUnderCacheLock(ctx context.Context, write func(ctx context.Context) error) {
	st := &c.st.disk
	st.write.Lock()
	defer st.write.Unlock()
	path := c.providerModelsCachePath()
	wait, cancel := context.WithTimeout(ctx, providerModelsLockWait)
	defer cancel()
	lock, err := fileutil.OpenLock(wait, home.ProviderModelsLock.In(c.home), fileutil.RecordHolder(), fileutil.Range(0, 1),
		fileutil.TimeoutMessage(fmt.Sprintf("Timed out waiting for the provider models cache lock (%s)", path)))
	if err != nil {
		slog.Debug("provider models cache write skipped", "path", path, "error", err)
		return
	}
	defer func() { _ = lock.Release() }()
	if err := write(lock.Context(ctx)); err != nil {
		slog.Debug("provider models cache write skipped", "path", path, "error", err)
	}
}

func (c *Catalog) storeCacheEntry(ctx context.Context, cacheKey string, entry cacheEntry) {
	c.writeUnderCacheLock(ctx, func(ctx context.Context) error {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		cache := c.loadProviderModelsCache()
		cache[cacheKey] = encoded
		return fileutil.WriteJSON(ctx, c.providerModelsCachePath(), cache, fileutil.JSONStyle{})
	})
}

func liveResultEntry(fp string, live listing, existing cacheRow, at float64) (cacheEntry, bool) {
	entry := newCacheEntry(fp, live.ids, at)
	if !live.fallback {
		return entry, true
	}
	if existing.validFor(fp, false) && !existing.fallback {
		return cacheEntry{}, false
	}
	entry.Fallback = true
	return entry, true
}

func (c *Catalog) ollamaNativeProbeReachable(ctx context.Context) bool {
	baseURL, err := c.local.OllamaBaseURL(ctx)
	if err != nil {
		return false
	}
	headers, err := c.local.OllamaNativeHeaders(ctx, baseURL, local.ProbeKey{})
	if err != nil {
		return false
	}
	native, err := c.local.ShouldUseOllamaNativeCatalog(ctx, providerOllama, baseURL, headers)
	if err != nil || !native {
		return false
	}
	_, reachable, err := c.local.ProbeOllamaLocalModels(ctx, baseURL, local.OllamaProbeTimeout, headers)
	return err == nil && reachable
}

func fileVersion(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	digest := "<unreadable>"
	if data, err := os.ReadFile(path); err == nil {
		digest = blake2bHex(data, 16)
	}
	inode, changed := statIdentity(path, info)
	return "(" + strconv.FormatInt(info.ModTime().UnixNano(), 10) + ", " + strconv.FormatInt(info.Size(), 10) + ", " +
		strconv.FormatUint(inode, 10) + ", " + strconv.FormatInt(changed, 10) + ", '" + digest + "')", nil
}

func asciiJSONString(text string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range text {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= 0x20 && r <= 0x7e:
			b.WriteRune(r)
		case r > 0xffff:
			high, low := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, high, low)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func spacedJSONObject(fields map[string]string) string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, asciiJSONString(name)+": "+asciiJSONString(fields[name]))
	}
	return "{" + strings.Join(pairs, ", ") + "}"
}

func (c *Catalog) rawEnv(ctx context.Context, name string) string {
	value, err := c.secrets.Get(ctx, name, "")
	if err != nil {
		slog.Debug("environment value is unreadable for a catalog lookup", "name", name, "error", err)
		return ""
	}
	return value
}

func (c *Catalog) sameRegisteredProvider(ctx context.Context, provider, configured string) bool {
	if provider == configured {
		return true
	}
	row, known := c.reg.RegistryRow(ctx, provider)
	other, configuredKnown := c.reg.RegistryRow(ctx, configured)
	return known && configuredKnown && row.ID == other.ID
}

func (c *Catalog) credentialFingerprint(ctx context.Context, provider string) string {
	var parts []string
	envPart := func(name string) {
		if name != "" {
			parts = append(parts, name+"="+c.rawEnv(ctx, name))
		}
	}
	if row, ok := c.reg.RegistryRow(ctx, provider); ok {
		for _, name := range row.APIKeyEnvVars {
			envPart(name)
		}
		envPart(row.BaseURLEnvVar)
	}
	if p, ok := c.reg.Get(ctx, provider); ok && p.Spec().AuthTypeOrDefault() == auth.AuthTypeExternalProcess {
		for _, name := range p.Spec().Process.CommandEnv {
			envPart(name)
		}
		envPart(p.Spec().Process.ArgsEnv)
	}
	model := c.modelConfig(ctx)
	if provider == "openai" || provider == "openai-api" {
		parts = append(parts, "effective_base="+c.openAIDiscoveryBaseURL(ctx, provider))
	}
	if provider == providerAzureFoundry {
		effective := ""
		if c.sameRegisteredProvider(ctx, providerAzureFoundry, strings.ToLower(strings.TrimSpace(model.Provider))) {
			effective = strings.TrimRight(strings.TrimSpace(model.BaseURL), "/")
		}
		parts = append(parts, "effective_base="+effective)
	}
	if provider == providerOllama {
		parts = append(parts, c.ollamaFingerprintParts(ctx, model.Provider, model.BaseURL)...)
	}
	filePart := func(label, path string) {
		version, err := fileVersion(path)
		switch {
		case err == nil:
			parts = append(parts, label+"@"+version)
		case errors.Is(err, fs.ErrNotExist):
			parts = append(parts, label+"@missing")
		}
	}
	if provider == "openai-codex" {
		parts = append(parts, "codex_identity="+c.codexCatalogCredentialIdentity(ctx))
	} else {
		filePart(string(home.AuthFile), home.AuthFile.In(c.home))
		filePart(string(home.CredentialsFile), home.CredentialsFile.In(c.home))
		files := []string{"~/.codex/auth.json", "~/.claude/.credentials.json"}
		for _, p := range c.reg.All(ctx) {
			files = append(files, p.Spec().CatalogCredentialFiles...)
		}
		files = append(files, "~/.minimax/credentials.json")
		for _, rel := range files {
			path := home.ExpandUser(rel)
			filePart(path, path)
		}
	}
	return blake2bHex8([]byte(strings.Join(parts, "|")))
}

func (c *Catalog) ollamaFingerprintParts(ctx context.Context, modelProvider, modelBaseURL string) []string {
	var entry struct {
		baseURL, apiKey, keyEnv string
		headers                 map[string]string
	}
	if cfg := c.config(ctx); cfg != nil {
		if configured, ok := cfg.Providers.Get(providerOllama); ok {
			entry.baseURL = firstNonEmpty(configured.BaseURL, configured.API, configured.URL)
			entry.apiKey = configured.APIKey
			entry.keyEnv = firstNonEmpty(configured.KeyEnv, configured.APIKeyEnv)
			entry.headers = map[string]string{}
			for _, name := range configured.ExtraHeaders.Keys() {
				entry.headers[name], _ = configured.ExtraHeaders.Get(name)
			}
		}
	}
	parts := []string{
		"OLLAMA_HOST=" + c.rawEnv(ctx, "OLLAMA_HOST"),
		"providers.ollama.base_url=" + entry.baseURL,
		"providers.ollama.api_key=" + entry.apiKey,
		"providers.ollama.key_env=" + entry.keyEnv,
	}
	if entry.keyEnv != "" {
		parts = append(parts, entry.keyEnv+"="+c.rawEnv(ctx, entry.keyEnv))
	}
	return append(parts,
		"model.provider="+modelProvider+"|model.base_url="+modelBaseURL,
		"providers.ollama.extra_headers="+spacedJSONObject(entry.headers))
}

func (c *Catalog) spawnCacheRefresh(ctx context.Context, cacheKey string, refresh func(ctx context.Context) (cacheEntry, bool)) {
	st := &c.st.disk
	st.mu.Lock()
	if st.refreshing[cacheKey] {
		st.mu.Unlock()
		return
	}
	st.refreshing[cacheKey] = true
	st.mu.Unlock()
	done := func() {
		st.mu.Lock()
		delete(st.refreshing, cacheKey)
		st.mu.Unlock()
	}
	started := c.background(ctx, func(bg context.Context) {
		defer done()
		if entry, ok := refresh(bg); ok {
			c.storeCacheEntry(bg, cacheKey, entry)
		}
	})
	if !started {
		done()
	}
}

func (c *Catalog) refreshProviderRow(ctx context.Context, cacheKey string) (cacheEntry, bool) {
	live, err := c.providerModelIDs(ctx, cacheKey, true)
	if err != nil {
		slog.Debug("provider models cache refresh failed", "provider", cacheKey, "error", err)
		return cacheEntry{}, false
	}
	if len(live.ids) == 0 && !(cacheKey == providerOllama && c.ollamaNativeProbeReachable(ctx)) {
		return cacheEntry{}, false
	}
	fp := c.credentialFingerprint(ctx, cacheKey)
	return liveResultEntry(fp, live, parseCacheRow(c.loadProviderModelsCache()[cacheKey]), unixSeconds(time.Now()))
}

func (c *Catalog) normalizedCacheSlug(ctx context.Context, provider string) string {
	requested := strings.ToLower(strings.TrimSpace(provider))
	if requested == providerOllama {
		return requested
	}
	if normalized := c.normalizeProvider(ctx, provider); normalized != "" {
		return normalized
	}
	return provider
}

func (c *Catalog) withoutAccountGated(ctx context.Context, provider string, models []string) []string {
	slug := c.normalizedCacheSlug(ctx, provider)
	gated := slug == "openai" || slug == "openai-api" || slug == "openai-codex"
	out := make([]string, 0, len(models))
	for _, model := range models {
		if !(gated && wire.IsAstraModel(model)) {
			out = append(out, model)
		}
	}
	return out
}

func (c *Catalog) cachedProviderModelIDs(ctx context.Context, provider string, force, nonBlocking bool) ([]string, error) {
	normalized := c.normalizedCacheSlug(ctx, provider)
	if normalized == "" {
		return []string{}, nil
	}
	ollama := normalized == providerOllama
	fp := c.credentialFingerprint(ctx, normalized)
	row := parseCacheRow(c.loadProviderModelsCache()[normalized])
	now := unixSeconds(time.Now())
	refresh := func(bg context.Context) (cacheEntry, bool) { return c.refreshProviderRow(bg, normalized) }
	if !force {
		if tier := diskServeTier(row, fp, now, ollama, providerModelsCacheTTL); tier != "" {
			if tier == "stale" {
				c.spawnCacheRefresh(ctx, normalized, refresh)
			}
			return c.WithoutGenerationModels(slices.Clone(row.models)), nil
		}
	}
	if nonBlocking && !force {
		c.spawnCacheRefresh(ctx, normalized, refresh)
		if row.validFor(fp, ollama) {
			return c.WithoutGenerationModels(c.withoutAccountGated(ctx, normalized, row.models)), nil
		}
		return []string{}, nil
	}
	live, err := c.providerModelIDs(ctx, normalized, force)
	if err != nil {
		return nil, err
	}
	if len(live.ids) > 0 {
		fresh, store := liveResultEntry(fp, live, row, now)
		if !store {
			return c.WithoutGenerationModels(c.withoutAccountGated(ctx, normalized, row.models)), nil
		}
		c.storeCacheEntry(ctx, normalized, fresh)
		return c.WithoutGenerationModels(slices.Clone(live.ids)), nil
	}
	if ollama {
		if c.ollamaNativeProbeReachable(ctx) {
			c.storeCacheEntry(ctx, normalized, newCacheEntry(fp, nil, now))
			return []string{}, nil
		}
		if row.valid && row.fp == fp && len(row.models) > 0 {
			return c.WithoutGenerationModels(slices.Clone(row.models)), nil
		}
		return []string{}, nil
	}
	if row.validFor(fp, false) {
		return c.WithoutGenerationModels(c.withoutAccountGated(ctx, normalized, row.models)), nil
	}
	return []string{}, nil
}

func (c *Catalog) CachedProviderModelIDs(ctx context.Context, provider string, force, nonBlocking bool) ([]string, error) {
	return c.cachedProviderModelIDs(c.scoped(ctx), provider, force, nonBlocking)
}

func customEndpointFingerprint(key auth.Credential, mode string, headers map[string]string) string {
	identity := key.Key.Reveal()
	if source, ok := key.Tokens.(*auth.CommandTokenSource); ok && source != nil {
		identity = source.CacheIdentity()
	}
	return blake2bHex8([]byte(strings.Join([]string{identity, mode, spacedJSONObject(headers)}, "|")))
}

func (c *Catalog) cachedAPIModels(ctx context.Context, q APIModelsQuery) (ids []string, native, listed bool) {
	if q.Timeout <= 0 {
		q.Timeout = apiModelsTimeout
	}
	if q.TTL <= 0 {
		q.TTL = providerModelsCacheTTL
	}
	live := func(ctx context.Context) (listing, bool) {
		if q.Fetch != nil {
			fetched, isNative, ok := q.Fetch(ctx)
			return listing{ids: fetched, native: isNative}, ok
		}
		fetched, ok := c.fetchAPIModels(ctx, q.Key.ProbeKey(ctx), q.BaseURL, q.Timeout, q.Mode, q.Headers)
		return listing{ids: fetched}, ok
	}
	serve := func(rows listing, ok bool) ([]string, bool, bool) {
		if !ok {
			return nil, false, false
		}
		return c.WithoutGenerationModels(rows.ids), rows.native, true
	}
	normalizedURL := strings.ToLower(strings.TrimRight(strings.TrimSpace(q.BaseURL), "/"))
	if normalizedURL == "" {
		if q.CacheOnly {
			return nil, false, false
		}
		return serve(live(ctx))
	}
	fp := customEndpointFingerprint(q.Key, q.Mode, q.Headers)
	cacheKey := "custom:" + normalizedURL + "#" + fp
	row := parseCacheRow(c.loadProviderModelsCache()[cacheKey])
	now := unixSeconds(time.Now())
	entryOf := func(rows listing, at float64) cacheEntry {
		entry := newCacheEntry(fp, rows.ids, at)
		isNative := rows.native
		entry.Native = &isNative
		return entry
	}
	fromRow := listing{ids: slices.Clone(row.models), native: row.native}
	if !q.Force && row.validFor(fp, row.native) {
		age := now - row.at
		if age < q.TTL.Seconds() {
			return serve(fromRow, true)
		}
		if len(row.models) > 0 && age < providerModelsStaleServeMax.Seconds() {
			c.spawnCacheRefresh(ctx, cacheKey, func(bg context.Context) (cacheEntry, bool) {
				rows, ok := live(bg)
				if !ok || (len(rows.ids) == 0 && !rows.native) {
					return cacheEntry{}, false
				}
				return entryOf(rows, unixSeconds(time.Now())), true
			})
			return serve(fromRow, true)
		}
	}
	if q.CacheOnly {
		return nil, false, false
	}
	rows, ok := live(ctx)
	if ok && (len(rows.ids) > 0 || rows.native) {
		c.storeCacheEntry(ctx, cacheKey, entryOf(rows, now))
		return serve(rows, true)
	}
	if row.validFor(fp, false) {
		return serve(fromRow, true)
	}
	return serve(rows, ok)
}

func (c *Catalog) CachedAPIModels(ctx context.Context, q APIModelsQuery) (ids []string, native, listed bool) {
	return c.cachedAPIModels(c.scoped(ctx), q)
}

func (c *Catalog) ClearModelsCache(ctx context.Context, provider string) {
	ctx = c.scoped(ctx)
	c.clearManifestBackoff()
	c.local.ResetOllamaProbeCaches()
	for _, p := range c.reg.All(ctx) {
		if invalidator, ok := p.(providers.CatalogInvalidator); ok {
			invalidator.InvalidateCatalog()
		}
	}
	c.writeUnderCacheLock(ctx, func(ctx context.Context) error {
		path := c.providerModelsCachePath()
		if provider == "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		}
		cache := c.loadProviderModelsCache()
		normalized := c.normalizedCacheSlug(ctx, provider)
		if _, ok := cache[normalized]; !ok {
			return nil
		}
		delete(cache, normalized)
		return fileutil.WriteJSON(ctx, path, cache, fileutil.JSONStyle{})
	})
}
