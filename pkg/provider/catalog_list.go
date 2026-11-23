package provider

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/settings"
)

// DefaultCatalogURL is the site copy of the shipped model catalog the refresh fetches
const DefaultCatalogURL = "https://icebath.app/api/model-catalog.json"

// DefaultCatalogFallbackURLs hold the same manifest at its repository path
var DefaultCatalogFallbackURLs = []string{
	"https://raw.githubusercontent.com/nickheyer/icebath/main/pkg/provider/model-catalog.json",
}

// Repository path of the shipped manifest, also the file the site bundle publishes
const shippedCatalogPath = "pkg/provider/model-catalog.json"

//go:embed model-catalog.json
var shippedCatalogJSON []byte

const (
	DefaultCatalogTTLMinutes = 20.0
	DefaultCatalogTTLHours   = DefaultCatalogTTLMinutes / 60.0
	DefaultCatalogTimeout    = 8 * time.Second
	// CatalogFetchRetryDelay keeps a failed URL unasked in this process after a fetch with nothing cached
	CatalogFetchRetryDelay  = 300 * time.Second
	SupportedCatalogVersion = 1
)

// CatalogConfig is the model_catalog config block with defaults filled in
type CatalogConfig struct {
	Enabled   bool
	URL       string
	TTLHours  float64
	Providers map[string]any
}

type catalogBackoffKey struct{ home, kind, url string }

var (
	catalogCacheMu          sync.Mutex
	catalogCache            map[string]any
	catalogCacheSourceMtime time.Time
	catalogCacheSourcePath  string
	catalogRetryAfter       = map[catalogBackoffKey]time.Time{}
	catalogRetryMu          sync.Mutex
	catalogSWRMu            sync.Mutex
	catalogSWRInflight      = map[string]bool{}
	catalogSWRWait          sync.WaitGroup
)

// Clock behind the catalog backoff window and freshness checks
var catalogClock = time.Now

// Manifest fetch seam, one URL per call
var fetchCatalogManifest = fetchCatalogManifestHTTP

// Config seam so pickers can read a prepared block instead of the home config file
var loadCatalogConfig = loadCatalogConfigFromHome

// Sibling slice seams the refresh command drives
var (
	fetchOpenRouterModelsFn = fetchOpenRouterModels
)

func catalogHomeKey(ctx context.Context) string { return home.Key(home.Dir(ctx)) }

func catalogBackingOff(ctx context.Context, kind, url string) bool {
	key := catalogBackoffKey{catalogHomeKey(ctx), kind, url}
	catalogRetryMu.Lock()
	defer catalogRetryMu.Unlock()
	return catalogClock().Before(catalogRetryAfter[key])
}

func noteCatalogFetchResult(ctx context.Context, kind, url string, ok bool) {
	key := catalogBackoffKey{catalogHomeKey(ctx), kind, url}
	catalogRetryMu.Lock()
	defer catalogRetryMu.Unlock()
	if ok {
		delete(catalogRetryAfter, key)
		return
	}
	catalogRetryAfter[key] = catalogClock().Add(CatalogFetchRetryDelay)
}

// ClearCatalogFetchBackoff forgets the active profile's fetch failures so an explicit refresh retries every URL
func ClearCatalogFetchBackoff(ctx context.Context) {
	h := catalogHomeKey(ctx)
	catalogRetryMu.Lock()
	defer catalogRetryMu.Unlock()
	for key := range catalogRetryAfter {
		if key.home == h {
			delete(catalogRetryAfter, key)
		}
	}
}

func floatOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func boolOf(v any, fallback bool) bool {
	switch b := v.(type) {
	case nil:
		return fallback
	case bool:
		return b
	case string:
		return b != ""
	}
	if f, ok := floatOf(v); ok {
		return f != 0
	}
	return true
}

// Reads the bound home's model_catalog settings, a refresh interval below one minute or a blank address falling back to the built-in one
func loadCatalogConfigFromHome(ctx context.Context) CatalogConfig {
	ttl := settings.ModelCatalogTtlMinutes.Get(ctx)
	if ttl <= 0 {
		ttl = time.Duration(DefaultCatalogTTLMinutes * float64(time.Minute))
	}
	url := strings.TrimSpace(settings.ModelCatalogUrl.Get(ctx))
	if url == "" {
		url = DefaultCatalogURL
	}
	return CatalogConfig{Enabled: settings.ModelCatalogEnabled.Get(ctx), URL: url, TTLHours: ttl.Hours(), Providers: settings.ModelCatalogProviders.Get(ctx)}
}

// CatalogCachePath is the per profile disk cache file
func CatalogCachePath(ctx context.Context) string {
	return filepath.Join(home.CacheDir(home.Dir(ctx)), "model_catalog.json")
}

// Reads JSON from a response body with a byte order mark tolerated
func decodeJSONBody(raw []byte) (any, error) {
	var data any
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

// urlopen GET of a manifest URL returning a validated manifest or nil on any failure
func fetchCatalogManifestHTTP(url string, timeout time.Duration) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := urllibOpen(ctx, urllibCall{url: url, timeout: timeout,
		headers: []pyhttpx.HeaderLine{{Name: "Accept", Value: "application/json"}, {Name: "User-Agent", Value: userAgent("icebath-cli")}}})
	if err != nil {
		pkgLog().Info("model catalog fetch failed", "url", url, "error", err)
		return nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		pkgLog().Info("model catalog fetch failed", "url", url, "error", err)
		return nil
	}
	if resp.StatusCode >= 400 {
		pkgLog().Info("model catalog fetch failed", "url", url, "status", resp.StatusCode)
		return nil
	}
	data, err := decodeJSONBody(raw)
	if err != nil {
		pkgLog().Info("model catalog fetch failed", "url", url, "error", err)
		return nil
	}
	if !ValidateCatalogManifest(data) {
		pkgLog().Info("model catalog failed schema validation", "url", url)
		return nil
	}
	m, _ := asMap(data)
	return m
}

// First manifest that fetches from the primary then the fallbacks, skipping any equal to the primary
func fetchCatalogManifestWithFallback(primary string, timeout time.Duration, fallbacks []string) map[string]any {
	if data := fetchCatalogManifest(primary, timeout); data != nil {
		return data
	}
	for _, url := range fallbacks {
		if url == "" || url == primary {
			continue
		}
		if data := fetchCatalogManifest(url, timeout); data != nil {
			pkgLog().Info("model catalog primary URL failed, using fallback", "url", url)
			return data
		}
	}
	return nil
}

// Integer value of a decoded JSON scalar, floats and strings rejected like a Python int check
func jsonIntOf(v any) (int64, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// ValidateCatalogManifest reports whether data has the minimum manifest shape at a known schema version
func ValidateCatalogManifest(data any) bool {
	m, ok := asMap(data)
	if !ok {
		return false
	}
	version, ok := jsonIntOf(m["version"])
	if !ok || version > SupportedCatalogVersion {
		return false
	}
	providers, ok := asMap(m["providers"])
	if !ok {
		return false
	}
	for _, pblock := range providers {
		block, ok := asMap(pblock)
		if !ok {
			return false
		}
		models, ok := asList(block["models"])
		if !ok {
			return false
		}
		for _, mm := range models {
			entry, ok := asMap(mm)
			if !ok {
				return false
			}
			id, ok := entry["id"].(string)
			if !ok || strings.TrimSpace(id) == "" {
				return false
			}
		}
	}
	return true
}

// Disk cache contents and modification time, nil and zero when missing or invalid
func readCatalogDiskCache(ctx context.Context) (map[string]any, time.Time) {
	path := CatalogCachePath(ctx)
	st, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}
	}
	data, err := decodeJSONBody(raw)
	if err != nil || !ValidateCatalogManifest(data) {
		return nil, time.Time{}
	}
	m, _ := asMap(data)
	return m, st.ModTime()
}

func writeCatalogDiskCache(ctx context.Context, data map[string]any) {
	if err := atomicJSONWrite(CatalogCachePath(ctx), data); err != nil {
		pkgLog().Info("model catalog cache write failed", "error", err)
	}
}

// Refreshes the manifest off thread, at most one in flight per cache file
func spawnCatalogSWRRefresh(ctx context.Context, url string) {
	key := CatalogCachePath(ctx)
	catalogSWRMu.Lock()
	if catalogSWRInflight[key] {
		catalogSWRMu.Unlock()
		return
	}
	catalogSWRInflight[key] = true
	catalogSWRMu.Unlock()
	refreshCtx := home.WithOverride(context.Background(), home.Dir(ctx))
	catalogSWRWait.Add(1)
	go func() {
		defer catalogSWRWait.Done()
		defer func() {
			catalogSWRMu.Lock()
			delete(catalogSWRInflight, key)
			catalogSWRMu.Unlock()
		}()
		if fetched := fetchCatalogManifestWithFallback(url, DefaultCatalogTimeout, DefaultCatalogFallbackURLs); fetched != nil {
			writeCatalogDiskCache(refreshCtx, fetched)
		}
	}()
}

func rememberCatalog(ctx context.Context, data map[string]any, mtime time.Time) map[string]any {
	catalogCacheMu.Lock()
	defer catalogCacheMu.Unlock()
	catalogCache, catalogCacheSourceMtime = data, mtime
	catalogCacheSourcePath = CatalogCachePath(ctx)
	return data
}

// The in process copy when it mirrors the active profile's cache file
func inProcessCatalog(ctx context.Context) (map[string]any, time.Time, bool) {
	catalogCacheMu.Lock()
	defer catalogCacheMu.Unlock()
	if catalogCache != nil && catalogCacheSourcePath == CatalogCachePath(ctx) {
		return catalogCache, catalogCacheSourceMtime, true
	}
	return nil, time.Time{}, false
}

// Manifest compiled into the binary, a fresh copy per call, the floor every catalog read falls back to
func shippedCatalog() map[string]any {
	data, err := decodeJSONBody(shippedCatalogJSON)
	if err != nil || !ValidateCatalogManifest(data) {
		panic("provider: shipped model catalog is not a valid manifest")
	}
	m, _ := asMap(data)
	return m
}

// GetCatalog returns the fetched or cached manifest, else the shipped one, serving a stale disk copy while refreshing off thread
func GetCatalog(ctx context.Context, forceRefresh bool) map[string]any {
	cfg := loadCatalogConfig(ctx)
	if !cfg.Enabled {
		return map[string]any{}
	}
	ttl := time.Duration(cfg.TTLHours * float64(time.Hour))
	if ttl < 0 {
		ttl = 0
	}
	diskData, diskMtime := readCatalogDiskCache(ctx)
	now := catalogClock()
	diskFresh := diskData != nil && now.Sub(diskMtime) < ttl
	if !forceRefresh && diskData != nil {
		cached, cachedMtime, ok := inProcessCatalog(ctx)
		if diskFresh && ok && diskMtime.Equal(cachedMtime) {
			return cached
		}
		if !diskFresh {
			spawnCatalogSWRRefresh(ctx, cfg.URL)
		}
		return rememberCatalog(ctx, diskData, diskMtime)
	}
	if !forceRefresh && diskData == nil && catalogBackingOff(ctx, "manifest", cfg.URL) {
		return shippedCatalog()
	}
	fetched := fetchCatalogManifestWithFallback(cfg.URL, DefaultCatalogTimeout, DefaultCatalogFallbackURLs)
	noteCatalogFetchResult(ctx, "manifest", cfg.URL, fetched != nil)
	if fetched != nil {
		writeCatalogDiskCache(ctx, fetched)
		if newData, newMtime := readCatalogDiskCache(ctx); newData != nil {
			return rememberCatalog(ctx, newData, newMtime)
		}
		return rememberCatalog(ctx, fetched, now)
	}
	if diskData != nil {
		return rememberCatalog(ctx, diskData, diskMtime)
	}
	return shippedCatalog()
}

// CatalogRefreshInterval is the configured TTL as the gateway poll cadence, at least one minute
func CatalogRefreshInterval(ctx context.Context) time.Duration {
	d := time.Duration(loadCatalogConfig(ctx).TTLHours * float64(time.Hour))
	if d < time.Minute {
		return time.Minute
	}
	return d
}

// RefreshCatalogs force refreshes the manifest and the OpenRouter live list
func RefreshCatalogs(ctx context.Context, secrets *config.Secrets) bool {
	if !loadCatalogConfig(ctx).Enabled {
		return false
	}
	ClearCatalogFetchBackoff(ctx)
	catalog := GetCatalog(ctx, true)
	fetchOpenRouterModelsFn(ctx, true)
	return len(catalog) > 0
}

// Fetches a provider's override manifest when model_catalog.providers.<name>.url is set
func fetchCatalogProviderOverride(ctx context.Context, provider string) map[string]any {
	cfg := loadCatalogConfig(ctx)
	if !cfg.Enabled {
		return nil
	}
	providerCfg, ok := asMap(cfg.Providers[provider])
	if !ok {
		return nil
	}
	override, ok := providerCfg["url"].(string)
	if !ok || strings.TrimSpace(override) == "" {
		return nil
	}
	override = strings.TrimSpace(override)
	if catalogBackingOff(ctx, "override", override) {
		return nil
	}
	data := fetchCatalogManifest(override, DefaultCatalogTimeout)
	noteCatalogFetchResult(ctx, "override", override, data != nil)
	return data
}

func catalogBlockOf(manifest map[string]any, provider string) map[string]any {
	providers, _ := asMap(manifest["providers"])
	block, ok := asMap(providers[provider])
	if !ok {
		return nil
	}
	return block
}

// Provider block from the override manifest first then the main manifest
func catalogProviderBlock(ctx context.Context, provider string) map[string]any {
	if block := catalogBlockOf(fetchCatalogProviderOverride(ctx, provider), provider); block != nil {
		return block
	}
	return catalogBlockOf(GetCatalog(ctx, false), provider)
}

type catalogEntry struct {
	id    string
	entry map[string]any
}

// Every model entry of a block with a non empty id
func catalogBlockIDs(block map[string]any) []catalogEntry {
	models, _ := asList(block["models"])
	var out []catalogEntry
	for _, m := range models {
		entry, ok := asMap(m)
		if !ok {
			continue
		}
		id := strings.TrimSpace(stringOf(entry["id"]))
		if id == "" {
			continue
		}
		out = append(out, catalogEntry{id, entry})
	}
	return out
}

// CuratedOpenRouterModels returns OpenRouter's curated rows from the manifest, nil when it has none
func CuratedOpenRouterModels(ctx context.Context) []CatalogModel {
	var out []CatalogModel
	for _, row := range catalogBlockIDs(catalogProviderBlock(ctx, "openrouter")) {
		out = append(out, CatalogModel{ID: row.id, Description: stringOf(row.entry["description"])})
	}
	return out
}

func defaultModelFromBlock(block map[string]any) string {
	for _, row := range catalogBlockIDs(block) {
		if boolOf(row.entry["default"], false) {
			return row.id
		}
	}
	return ""
}

// DefaultModelFromCatalogCache returns the manifest's labeled default from memory, disk, then the shipped manifest, never a fetch
func DefaultModelFromCatalogCache(ctx context.Context, provider string) string {
	if cached, _, ok := inProcessCatalog(ctx); ok {
		if found := defaultModelFromBlock(catalogBlockOf(cached, provider)); found != "" {
			return found
		}
	}
	diskData, _ := readCatalogDiskCache(ctx)
	if diskData == nil {
		diskData = shippedCatalog()
	}
	return defaultModelFromBlock(catalogBlockOf(diskData, provider))
}

// SeedCatalogCacheFromCheckout overwrites the disk cache with the checkout's shipped manifest
func SeedCatalogCacheFromCheckout(ctx context.Context, projectRoot string) bool {
	src := filepath.Join(projectRoot, filepath.FromSlash(shippedCatalogPath))
	raw, err := os.ReadFile(src)
	if err != nil {
		pkgLog().Debug("model catalog seed from checkout skipped", "path", src, "error", err)
		return false
	}
	data, err := decodeJSONBody(raw)
	if err != nil {
		pkgLog().Debug("model catalog seed from checkout skipped", "path", src, "error", err)
		return false
	}
	if !ValidateCatalogManifest(data) {
		pkgLog().Debug("model catalog seed from checkout skipped, invalid manifest", "path", src)
		return false
	}
	m, _ := asMap(data)
	writeCatalogDiskCache(ctx, m)
	ResetCatalogCache()
	return true
}

// ResetCatalogCache clears the in process copy and every fetch failure backoff
func ResetCatalogCache() {
	catalogRetryMu.Lock()
	catalogRetryAfter = map[catalogBackoffKey]time.Time{}
	catalogRetryMu.Unlock()
	catalogCacheMu.Lock()
	defer catalogCacheMu.Unlock()
	catalogCache = nil
	catalogCacheSourceMtime = time.Time{}
	catalogCacheSourcePath = ""
}
