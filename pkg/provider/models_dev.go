package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// ModelsDevURL is the live registry endpoint
const ModelsDevURL = "https://models.dev/api.json"

const (
	modelsDevCacheTTL      = 4 * time.Hour
	modelsDevRetryDelay    = 5 * time.Minute
	modelsDevFetchDeadline = 15 * time.Second
	modelsDevConnectLimit  = 5 * time.Second
	modelsDevReadLimit     = 10 * time.Second
)

var modelsDevLog = catalogLog

// Clock behind the models.dev freshness and backoff windows
var modelsDevClock = time.Now

var (
	modelsDevStateMu         sync.Mutex
	modelsDevCache           map[string]any
	modelsDevCacheTime       time.Time
	modelsDevRetryAfter      time.Time
	modelsDevFetchMu         sync.Mutex
	modelsDevRefreshMu       sync.Mutex
	modelsDevRefreshInFlight bool
	modelsDevDownloadSlot    sync.Mutex
	modelsDevRefreshWait     sync.WaitGroup
	modelsDevOverrideWarned  = map[string]bool{}
	modelsDevUnknownWarned   = map[string]bool{}
	modelsDevWarnMu          sync.Mutex
	modelsDevToProviderOnce  sync.Once
	modelsDevToProvider      map[string][]string
	modelCapabilitiesMu      sync.Mutex
	registeredModelCaps      = map[string]map[string]map[string]any{}
)

// Network seam returning status, payload and ETag for one registry download
var downloadModelsDev = downloadModelsDevHTTP

// Whole download wall clock cap, a seam so tests can shrink it
var modelsDevFetchDeadlineFor = func() time.Duration { return modelsDevFetchDeadline }

// ModelsDevEntry is the full metadata of one models.dev model row
type ModelsDevEntry struct {
	ID               string
	Name             string
	Family           string
	ProviderID       string
	Reasoning        bool
	ToolCall         bool
	Attachment       bool
	Temperature      bool
	StructuredOutput bool
	OpenWeights      bool
	InputModalities  []string
	OutputModalities []string
	ContextWindow    int
	MaxOutput        int
	MaxInput         int
	CostInput        float64
	CostOutput       float64
	CostCacheRead    *float64
	CostCacheWrite   *float64
	KnowledgeCutoff  string
	ReleaseDate      string
	Status           string
	Interleaved      any
	Raw              map[string]any
}

// HasCostData reports whether either token price is known
func (m ModelsDevEntry) HasCostData() bool { return m.CostInput > 0 || m.CostOutput > 0 }

// SupportsVision reports image input through the attachment flag or the input modalities
func (m ModelsDevEntry) SupportsVision() bool {
	return m.Attachment || containsString(m.InputModalities, "image")
}

// SupportsPDF reports pdf among the input modalities
func (m ModelsDevEntry) SupportsPDF() bool { return containsString(m.InputModalities, "pdf") }

// SupportsAudioInput reports audio among the input modalities
func (m ModelsDevEntry) SupportsAudioInput() bool { return containsString(m.InputModalities, "audio") }

// FormatCapabilities renders the capability flags as a comma list or basic
func (m ModelsDevEntry) FormatCapabilities() string {
	flags := []struct {
		on    bool
		label string
	}{
		{m.Reasoning, "reasoning"}, {m.ToolCall, "tools"}, {m.SupportsVision(), "vision"}, {m.SupportsPDF(), "PDF"},
		{m.SupportsAudioInput(), "audio"}, {m.StructuredOutput, "structured output"}, {m.OpenWeights, "open weights"},
	}
	var out []string
	for _, f := range flags {
		if f.on {
			out = append(out, f.label)
		}
	}
	if len(out) == 0 {
		return "basic"
	}
	return strings.Join(out, ", ")
}

// ModelsDevProvider is the metadata of one models.dev provider row
type ModelsDevProvider struct {
	ID         string
	Name       string
	Env        []string
	API        string
	Doc        string
	ModelCount int
}

// ModelCapabilities is the structured capability view of a model, nil pointers mean unknown
type ModelCapabilities struct {
	SupportsTools     bool
	SupportsVision    *bool
	SupportsReasoning *bool
	ContextWindow     int
	MaxOutputTokens   int
	ModelFamily       string
}

// Static hermes provider ids to models.dev provider ids
var providerToModelsDev = map[string]string{
	"openrouter": "openrouter", "novita": "novita-ai", "anthropic": "anthropic",
	"openai": "openai", "openai-api": "openai", "openai-codex": "openai", "zai": "zai",
	"kimi": "kimi-for-coding", "kimi-coding": "kimi-for-coding",
	"moonshot": "kimi-for-coding", "stepfun": "stepfun",
	"kimi-coding-cn": "kimi-for-coding", "minimax": "minimax",
	"minimax-oauth": "minimax", "minimax-cn": "minimax-cn", "deepseek": "deepseek",
	"alibaba": "alibaba", "qwen-oauth": "alibaba",
	"ai-gateway": "vercel", "opencode-zen": "opencode",
	"opencode-go": "opencode-go",
	"kilocode":    "kilo", "fireworks": "fireworks-ai",
	"huggingface": "huggingface", "gemini": "google", "google": "google",
	"xai":       "xai",
	"xai-oauth": "xai",
	"xiaomi":    "xiaomi", "nvidia": "nvidia",
	"meta-ai": "meta", "meta": "meta", "groq": "groq", "mistral": "mistral",
	"togetherai": "togetherai", "perplexity": "perplexity", "cohere": "cohere",
	"ollama-cloud": "ollama-cloud",
}

// Explicit models.dev ids declared by registered profiles the static map lacks
func profileModelsDevIDs() map[string]string {
	out := map[string]string{}
	for _, p := range Profiles() {
		if p.ModelsDevID == "" || p.ModelsDevID == p.Name {
			continue
		}
		if _, static := providerToModelsDev[p.Name]; static {
			continue
		}
		out[p.Name] = p.ModelsDevID
	}
	return out
}

// ModelsDevProviderMap returns hermes provider id to models.dev id, static map then profile declarations
func ModelsDevProviderMap() map[string]string {
	out := make(map[string]string, len(providerToModelsDev))
	for k, v := range providerToModelsDev {
		out[k] = v
	}
	for k, v := range profileModelsDevIDs() {
		out[k] = v
	}
	return out
}

// ModelsDevIDFor returns the models.dev id for an exact hermes provider id
func ModelsDevIDFor(provider string) (string, bool) {
	key := strings.TrimSpace(provider)
	if key == "" {
		return "", false
	}
	if v, ok := providerToModelsDev[key]; ok {
		return v, true
	}
	v, ok := profileModelsDevIDs()[key]
	return v, ok
}

// Hermes provider ids that map to one models.dev id
func modelsDevToHermesIDs(mdevID string) []string {
	modelsDevToProviderOnce.Do(func() {
		modelsDevToProvider = map[string][]string{}
		for _, hermesID := range sortedStringKeys(providerToModelsDev) {
			mapped := providerToModelsDev[hermesID]
			modelsDevToProvider[mapped] = append(modelsDevToProvider[mapped], hermesID)
		}
	})
	out := cloneStrings(modelsDevToProvider[mdevID])
	profileIDs := profileModelsDevIDs()
	for _, name := range sortedStringKeys(profileIDs) {
		if profileIDs[name] == mdevID {
			out = append(out, name)
		}
	}
	return out
}

func dictOrEmpty(v any) map[string]any {
	m, ok := asMap(v)
	if !ok {
		return map[string]any{}
	}
	return m
}

// Nested config value, present false when any key on the path is absent
func cfgGet(cfg map[string]any, keys ...string) (any, bool) {
	var node any = cfg
	if cfg == nil {
		return nil, false
	}
	for _, key := range keys {
		m, ok := asMap(node)
		if !ok {
			return nil, false
		}
		v, ok := m[key]
		if !ok {
			return nil, false
		}
		node = v
	}
	return node, true
}

// Effective config tree of a home, nil when unreadable
func loadConfigTree(h string) map[string]any {
	tree, err := config.Load(h, nil)
	if err != nil {
		return nil
	}
	return map[string]any(tree)
}

// catalog_provider declared on a custom provider row or legacy custom_providers entry
func configuredCatalogProvider(provider string, cfg map[string]any) string {
	name := strings.TrimSpace(provider)
	if strings.HasPrefix(strings.ToLower(name), "custom:") {
		name = name[len("custom:"):]
	}
	if name == "" {
		return ""
	}
	if _, ok := ModelsDevIDFor(name); ok {
		return ""
	}
	providerCfg, _ := cfgGet(cfg, "providers", name)
	alias := stringOf(dictOrEmpty(providerCfg)["catalog_provider"])
	if alias == "" {
		legacy, _ := cfgGet(cfg, "custom_providers")
		if entries, ok := asList(legacy); ok {
			for _, e := range entries {
				entry, ok := asMap(e)
				if !ok || strings.TrimSpace(stringOf(entry["name"])) != name {
					continue
				}
				alias = stringOf(entry["catalog_provider"])
				break
			}
		}
	}
	return strings.TrimSpace(alias)
}

// models.dev provider id for a hermes provider, custom providers only through catalog_provider
func modelsDevID(h, provider string, cfg map[string]any) string {
	key := strings.TrimSpace(provider)
	if mdevID, ok := ModelsDevIDFor(key); ok {
		return mdevID
	}
	alias := configuredCatalogProvider(key, cfg)
	if alias == "" {
		return ""
	}
	mdevID := alias
	if mapped, ok := ModelsDevIDFor(alias); ok {
		mdevID = mapped
	}
	known := false
	for _, v := range ModelsDevProviderMap() {
		if v == mdevID {
			known = true
			break
		}
	}
	if !known {
		if _, inRegistry := fetchModelsDevIn(h, false)[mdevID]; !inRegistry {
			warnKey := key + "\x00" + alias
			modelsDevWarnMu.Lock()
			first := !modelsDevUnknownWarned[warnKey]
			modelsDevUnknownWarned[warnKey] = true
			modelsDevWarnMu.Unlock()
			if first {
				modelsDevLog.Warn(fmt.Sprintf("providers.%s: catalog_provider %q is neither a Hermes provider id nor a models.dev id; ignoring", key, alias))
			}
			return ""
		}
	}
	return mdevID
}

func modelsDevCachePath(h string) string {
	return filepath.Join(home.CacheDir(h), "models_dev_cache.json")
}

func modelsDevETagPath(h string) string {
	return filepath.Join(home.CacheDir(h), "models_dev_cache.etag")
}

// Last known ETag from disk, empty when missing
func loadModelsDevETag(h string) string {
	raw, err := os.ReadFile(modelsDevETagPath(h))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
}

func saveModelsDevETag(h, etag string) {
	path := modelsDevETagPath(h)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		modelsDevLog.Debug("Failed to save models.dev ETag", "error", err)
		return
	}
	if err := config.AtomicWrite(path, []byte(etag), 0o600); err != nil {
		modelsDevLog.Debug("Failed to save models.dev ETag", "error", err)
	}
}

// Deletes the ETag sidecar so the next fetch is unconditional
func clearModelsDevETag(h string) {
	if err := os.Remove(modelsDevETagPath(h)); err != nil && !errors.Is(err, os.ErrNotExist) {
		modelsDevLog.Debug("Failed to clear models.dev ETag", "error", err)
	}
}

// Registry URL honouring the models_dev.url config override
func modelsDevURL(h string) string {
	v, _ := cfgGet(loadConfigTree(h), "models_dev", "url")
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return ModelsDevURL
}

// A registry is servable when it is a non empty object
func validateModelsDevRegistry(data any) bool {
	m, ok := asMap(data)
	return ok && len(m) > 0
}

// Disk cache contents with a corrupt or empty file quarantined instead of served as empty
func loadModelsDevDiskCache(h string) map[string]any {
	path := modelsDevCachePath(h)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}
		}
		modelsDevLog.Warn("Failed to load models.dev disk cache; quarantining", "error", err)
		quarantineModelsDevCache(h, path)
		return map[string]any{}
	}
	var data any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")), &data); err != nil {
		modelsDevLog.Warn("Failed to load models.dev disk cache; quarantining", "error", err)
		quarantineModelsDevCache(h, path)
		return map[string]any{}
	}
	if validateModelsDevRegistry(data) {
		m, _ := asMap(data)
		return m
	}
	modelsDevLog.Warn("models.dev disk cache is corrupt or empty; quarantining (will refetch from network)")
	quarantineModelsDevCache(h, path)
	return map[string]any{}
}

// Renames a rejected cache aside and drops its ETag sidecar
func quarantineModelsDevCache(h, path string) {
	if err := os.Rename(path, strings.TrimSuffix(path, ".json")+".json.corrupt"); err != nil {
		modelsDevLog.Debug("Could not quarantine corrupt models.dev cache", "error", err)
	}
	clearModelsDevETag(h)
}

// Age of the disk cache, known false when missing or stamped in the future
func modelsDevDiskCacheAge(h string) (time.Duration, bool) {
	st, err := os.Stat(modelsDevCachePath(h))
	if err != nil {
		return 0, false
	}
	age := modelsDevClock().Sub(st.ModTime())
	if age < 0 {
		return 0, false
	}
	return age, true
}

func saveModelsDevDiskCache(h string, data map[string]any, etag string) {
	if err := atomicJSONWrite(modelsDevCachePath(h), data); err != nil {
		modelsDevLog.Debug("Failed to save models.dev disk cache", "error", err)
	}
	if etag != "" {
		saveModelsDevETag(h, etag)
	}
}

var errModelsDevNotModified = errors.New("models.dev registry not modified")

// ModelsDevFetchDeadlineError reports a download abandoned at the whole exchange cap
type ModelsDevFetchDeadlineError struct{ Timeout time.Duration }

func (e *ModelsDevFetchDeadlineError) Error() string {
	return fmt.Sprintf("models.dev fetch exceeded %s", e.Timeout)
}

var modelsDevHTTPClient = &http.Client{Transport: &http.Transport{
	DialContext:           (&net.Dialer{Timeout: modelsDevConnectLimit}).DialContext,
	TLSHandshakeTimeout:   modelsDevConnectLimit,
	ResponseHeaderTimeout: modelsDevReadLimit,
}}

// One registry GET with optional If-None-Match returning status, decoded payload and ETag
func downloadModelsDevHTTP(ctx context.Context, url string, headers map[string]string) (int, any, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, "", err
	}
	req.Header.Set("User-Agent", UserAgent("icebath-agent"))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := modelsDevHTTPClient.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return 304, nil, "", nil
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, nil, "", fmt.Errorf("models.dev returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, "", err
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return resp.StatusCode, nil, "", err
	}
	return resp.StatusCode, data, resp.Header.Get("ETag"), nil
}

// Live registry fetch bounded by one wall clock deadline with the single download slot held throughout
func fetchModelsDevFromNetwork(h string, conditional bool) (map[string]any, string, error) {
	headers := map[string]string{}
	if conditional {
		if etag := loadModelsDevETag(h); etag != "" {
			headers["If-None-Match"] = etag
		}
	}
	url := modelsDevURL(h)
	if !modelsDevDownloadSlot.TryLock() {
		return nil, "", errors.New("an abandoned models.dev download is still running")
	}
	type result struct {
		status int
		data   any
		etag   string
		err    error
	}
	deadline := modelsDevFetchDeadlineFor()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	done := make(chan result, 1)
	download := downloadModelsDev
	go func() {
		defer modelsDevDownloadSlot.Unlock()
		status, data, etag, err := download(ctx, url, headers)
		done <- result{status, data, etag, err}
	}()
	select {
	case r := <-done:
		cancel()
		if r.err != nil {
			return nil, "", r.err
		}
		if r.status == 304 {
			return nil, "", errModelsDevNotModified
		}
		if !validateModelsDevRegistry(r.data) {
			return nil, "", errors.New("models.dev returned an empty or invalid registry")
		}
		m, _ := asMap(r.data)
		return m, r.etag, nil
	case <-ctx.Done():
		cancel()
		return nil, "", &ModelsDevFetchDeadlineError{Timeout: deadline}
	}
}

// Moves the freshness stamp forward only so stale data gets a five minute grace before another refresh
func markModelsDevStaleGrace() {
	grace := modelsDevClock().Add(-modelsDevCacheTTL + modelsDevRetryDelay)
	if grace.After(modelsDevCacheTime) {
		modelsDevCacheTime = grace
	}
}

// Arms the grace window, kicks a background refresh and returns the held cache, state lock held
func serveModelsDevStale(h, msg string) map[string]any {
	markModelsDevStaleGrace()
	startBackgroundModelsDevRefresh(h)
	modelsDevLog.Debug(msg)
	return modelsDevCache
}

func modelsDevModelCount(data map[string]any) int {
	n := 0
	for _, p := range data {
		if pm, ok := asMap(p); ok {
			n += len(dictOrEmpty(pm["models"]))
		}
	}
	return n
}

// Persists a fetched registry to disk and memory and clears the backoff, fetch lock held
func commitModelsDevRegistry(h string, data map[string]any, etag, where string) {
	saveModelsDevDiskCache(h, data, etag)
	modelsDevStateMu.Lock()
	modelsDevCache = data
	modelsDevCacheTime = modelsDevClock()
	modelsDevRetryAfter = time.Time{}
	modelsDevStateMu.Unlock()
	modelsDevLog.Debug(fmt.Sprintf("Refreshed models.dev registry (%s): %d providers, %d total models", where, len(data), modelsDevModelCount(data)))
}

// After a 304 clears the backoff and re marks the held cache fresh, fetch lock held
func confirmModelsDevNotModified(h, where string) {
	modelsDevStateMu.Lock()
	defer modelsDevStateMu.Unlock()
	if len(modelsDevCache) == 0 {
		clearModelsDevETag(h)
		modelsDevRetryAfter = modelsDevClock().Add(modelsDevRetryDelay)
		modelsDevLog.Warn(fmt.Sprintf("models.dev returned 304 but no cached registry is held (%s); cleared ETag sidecar, will refetch unconditionally", where))
		return
	}
	modelsDevCacheTime = modelsDevClock()
	modelsDevRetryAfter = time.Time{}
	modelsDevLog.Debug(fmt.Sprintf("models.dev registry unchanged (304 Not Modified, %s); cache re-confirmed fresh", where))
}

// Arms the process wide five minute backoff, fetch lock held
func noteModelsDevRefreshFailure(err error, where string) {
	modelsDevStateMu.Lock()
	modelsDevRetryAfter = modelsDevClock().Add(modelsDevRetryDelay)
	modelsDevStateMu.Unlock()
	modelsDevLog.Debug(fmt.Sprintf("models.dev refresh failed (%s); retry suppressed for %ds: %v", where, int(modelsDevRetryDelay/time.Second), err))
}

// One conditional fetch plus state update, fetch lock held, nil when the fetch failed
func refreshModelsDevLocked(h, where string) map[string]any {
	modelsDevStateMu.Lock()
	conditional := len(modelsDevCache) > 0
	modelsDevStateMu.Unlock()
	data, etag, err := fetchModelsDevFromNetwork(h, conditional)
	switch {
	case err == nil:
		commitModelsDevRegistry(h, data, etag, where)
		return data
	case errors.Is(err, errModelsDevNotModified):
		confirmModelsDevNotModified(h, where)
		modelsDevStateMu.Lock()
		defer modelsDevStateMu.Unlock()
		return modelsDevCache
	default:
		noteModelsDevRefreshFailure(err, where)
		return nil
	}
}

func backgroundModelsDevRefresh(h string) {
	defer modelsDevRefreshWait.Done()
	defer func() {
		modelsDevRefreshMu.Lock()
		modelsDevRefreshInFlight = false
		modelsDevRefreshMu.Unlock()
	}()
	modelsDevFetchMu.Lock()
	defer modelsDevFetchMu.Unlock()
	refreshModelsDevLocked(h, "background")
}

// Starts one refresh worker when none runs and the failure backoff has elapsed, state lock held
func startBackgroundModelsDevRefresh(h string) {
	if modelsDevClock().Before(modelsDevRetryAfter) {
		return
	}
	modelsDevRefreshMu.Lock()
	if modelsDevRefreshInFlight {
		modelsDevRefreshMu.Unlock()
		return
	}
	modelsDevRefreshInFlight = true
	modelsDevRefreshMu.Unlock()
	modelsDevRefreshWait.Add(1)
	go backgroundModelsDevRefresh(h)
}

// WaitModelsDevRefresh blocks until any background registry refresh has finished
func WaitModelsDevRefresh() { modelsDevRefreshWait.Wait() }

// Registry served from memory, then disk of any age, then one singleflight network fetch
func fetchModelsDevIn(h string, allowNetwork bool) map[string]any {
	return fetchModelsDevWith(h, false, allowNetwork)
}

func fetchModelsDevWith(h string, forceRefresh, allowNetwork bool) map[string]any {
	if !allowNetwork {
		modelsDevStateMu.Lock()
		defer modelsDevStateMu.Unlock()
		if len(modelsDevCache) == 0 {
			if disk := loadModelsDevDiskCache(h); len(disk) > 0 {
				modelsDevCache = disk
				if age, ok := modelsDevDiskCacheAge(h); ok {
					modelsDevCacheTime = modelsDevClock().Add(-age)
				} else {
					modelsDevCacheTime = time.Time{}
				}
			}
		}
		return modelsDevCache
	}
	if !forceRefresh {
		modelsDevStateMu.Lock()
		if len(modelsDevCache) > 0 && modelsDevClock().Sub(modelsDevCacheTime) < modelsDevCacheTTL {
			defer modelsDevStateMu.Unlock()
			return modelsDevCache
		}
		if len(modelsDevCache) > 0 {
			defer modelsDevStateMu.Unlock()
			return serveModelsDevStale(h, "Using stale in-memory models.dev cache; refreshing in background")
		}
		if age, ok := modelsDevDiskCacheAge(h); ok {
			if disk := loadModelsDevDiskCache(h); len(disk) > 0 {
				modelsDevCache = disk
				if age >= modelsDevCacheTTL {
					defer modelsDevStateMu.Unlock()
					return serveModelsDevStale(h, fmt.Sprintf("Using stale models.dev disk cache (age=%.0fs); refreshing in background", age.Seconds()))
				}
				modelsDevCacheTime = modelsDevClock().Add(-age)
				modelsDevLog.Debug(fmt.Sprintf("Loaded models.dev from fresh disk cache (%d providers, age=%.0fs)", len(disk), age.Seconds()))
				defer modelsDevStateMu.Unlock()
				return modelsDevCache
			}
		}
		if modelsDevClock().Before(modelsDevRetryAfter) {
			defer modelsDevStateMu.Unlock()
			return modelsDevCache
		}
		modelsDevStateMu.Unlock()
	}
	modelsDevFetchMu.Lock()
	defer modelsDevFetchMu.Unlock()
	modelsDevStateMu.Lock()
	if !forceRefresh && (len(modelsDevCache) > 0 || modelsDevClock().Before(modelsDevRetryAfter)) {
		defer modelsDevStateMu.Unlock()
		return modelsDevCache
	}
	if forceRefresh && len(modelsDevCache) == 0 {
		if disk := loadModelsDevDiskCache(h); len(disk) > 0 {
			modelsDevCache = disk
			modelsDevCacheTime = time.Time{}
		}
	}
	modelsDevStateMu.Unlock()
	if served := refreshModelsDevLocked(h, "foreground"); served != nil {
		return served
	}
	modelsDevStateMu.Lock()
	defer modelsDevStateMu.Unlock()
	if len(modelsDevCache) == 0 {
		modelsDevCache = loadModelsDevDiskCache(h)
		modelsDevCacheTime = time.Time{}
		if len(modelsDevCache) > 0 {
			modelsDevLog.Debug(fmt.Sprintf("Loaded stale models.dev disk cache (%d providers)", len(modelsDevCache)))
		}
	}
	return modelsDevCache
}

// FetchModelsDev returns the registry keyed by provider id, empty on failure, with the network allowed
func FetchModelsDev(ctx context.Context, forceRefresh bool) map[string]any {
	return fetchModelsDevWith(home.Dir(ctx), forceRefresh, true)
}

// ModelsDevOffline returns any memory or disk copy of the registry without a request
func ModelsDevOffline(ctx context.Context) map[string]any {
	return fetchModelsDevIn(home.Dir(ctx), false)
}

// ResetModelsDevCache drops the in memory registry, the backoff and the once per process warnings
func ResetModelsDevCache() {
	modelsDevStateMu.Lock()
	modelsDevCache = nil
	modelsDevCacheTime = time.Time{}
	modelsDevRetryAfter = time.Time{}
	modelsDevStateMu.Unlock()
	modelsDevWarnMu.Lock()
	modelsDevOverrideWarned = map[string]bool{}
	modelsDevUnknownWarned = map[string]bool{}
	modelsDevWarnMu.Unlock()
}

// Raw models.dev provider entry or nil
func modelsDevRegistryProvider(h, mdevID string, allowNetwork bool) map[string]any {
	registry := fetchModelsDevWith(h, false, allowNetwork)
	p, ok := asMap(registry[mdevID])
	if !ok {
		return nil
	}
	return p
}

// The models map of a provider entry or nil
func modelsDevRegistryModels(h, mdevID string, allowNetwork bool) map[string]any {
	p := modelsDevRegistryProvider(h, mdevID, allowNetwork)
	if p == nil {
		return nil
	}
	models, ok := asMap(p["models"])
	if !ok {
		return nil
	}
	return models
}

// Models map for a hermes provider or nil when it is unknown to the registry
func modelsDevProviderModels(h, provider string, allowNetwork bool, cfg map[string]any) map[string]any {
	mdevID := modelsDevID(h, provider, cfg)
	if mdevID == "" {
		return nil
	}
	return modelsDevRegistryModels(h, mdevID, allowNetwork)
}

var openRouterCatalogProviders = map[string]bool{"openrouter": true}

// Base id to retry a lookup with when an OpenRouter routing variant suffix is present
func openRouterCatalogLookupBase(provider, model string) string {
	if !openRouterCatalogProviders[provider] {
		return ""
	}
	return OpenRouterVariantBase(model)
}

type modelsDevMatch struct {
	id    string
	entry map[string]any
}

// Candidate entries in order exact, case insensitive, cloud suffixed, then the OpenRouter base
func iterModelsDevEntries(models map[string]any, model string, suffixFallback bool, provider string) []modelsDevMatch {
	names := []string{model}
	if suffixFallback {
		names = append(names, model+":cloud", model+"-cloud")
	}
	var out []modelsDevMatch
	keys := sortedMapKeys(models)
	for _, name := range names {
		if entry, ok := asMap(models[name]); ok {
			out = append(out, modelsDevMatch{name, entry})
		}
		lower := strings.ToLower(name)
		for _, mid := range keys {
			if strings.ToLower(mid) != lower {
				continue
			}
			if entry, ok := asMap(models[mid]); ok {
				out = append(out, modelsDevMatch{mid, entry})
			}
		}
	}
	if base := openRouterCatalogLookupBase(provider, model); base != "" {
		out = append(out, iterModelsDevEntries(models, base, suffixFallback, "")...)
	}
	return out
}

// First catalog entry for a model or nil
func findModelsDevEntry(models map[string]any, model, provider string) map[string]any {
	for _, m := range iterModelsDevEntries(models, model, true, provider) {
		return m.entry
	}
	return nil
}

// Positive integer limit field of an entry or zero
func extractModelsDevLimit(entry any, key string) int {
	v := dictOrEmpty(dictOrEmpty(entry)["limit"])[key]
	if _, isBool := v.(bool); isBool {
		return 0
	}
	switch v.(type) {
	case float64, float32, int, int64, int32, uint, uint64, json.Number:
		f, _ := floatOf(v)
		if f > 0 {
			return int(f)
		}
	}
	return 0
}

func extractModelsDevContext(entry map[string]any) int {
	return extractModelsDevLimit(entry, "context")
}

// LookupModelsDevContext returns the context window with explicit overrides first and _default filling catalog misses
func LookupModelsDevContext(ctx context.Context, provider, model string, allowNetwork bool) (int, bool) {
	h := home.Dir(ctx)
	cfg := loadConfigTree(h)
	if n, ok := overrideContextWindow(provider, model, cfg); ok {
		return n, true
	}
	if n, ok := overrideInt(providerModelCapabilities(provider, model), "context_window"); ok {
		return n, true
	}
	if models := modelsDevProviderModels(h, provider, allowNetwork, cfg); models != nil {
		for _, m := range iterModelsDevEntries(models, model, true, provider) {
			if n := extractModelsDevContext(m.entry); n > 0 {
				return n, true
			}
		}
	}
	return defaultOverrideContext(provider, cfg)
}

// Safe base for models absent from the catalog
func unknownModelBase() map[string]any {
	return map[string]any{"limit": map[string]any{"context": float64(200000)}, "tool_call": true}
}

func deepseekFlashVision() map[string]any {
	return map[string]any{
		"limit":      map[string]any{"context": float64(1_000_000), "output": float64(384_000)},
		"modalities": map[string]any{"input": []any{"text", "image"}, "output": []any{"text"}},
		"tool_call":  true,
		"reasoning":  true,
		"family":     "deepseek-flash",
	}
}

// Built in metadata for account gated models the registry has not indexed yet
func builtinModelMetadataTable() map[[2]string]map[string]any {
	return map[[2]string]map[string]any{
		{"openai", "gpt-6-astra"}: {
			"limit":      map[string]any{"context": float64(1_050_000), "output": float64(128_000)},
			"modalities": map[string]any{"input": []any{"text", "image"}, "output": []any{"text"}},
			"tool_call":  true,
			"reasoning":  true,
			"family":     "gpt-6",
		},
		{"deepseek", "deepseek-flash"}:               deepseekFlashVision(),
		{"deepseek", "deepseek-v4-flash"}:            deepseekFlashVision(),
		{"deepseek", "deepseek-v4.1-flash"}:          deepseekFlashVision(),
		{"deepseek", "deepseek-v4-flash-vision-exp"}: deepseekFlashVision(),
	}
}

// The model_overrides config section, empty on any failure
func loadModelOverrides(cfg map[string]any) map[string]any {
	v, _ := cfgGet(cfg, "model_overrides")
	return dictOrEmpty(v)
}

// Override section for a provider keyed by hermes or models.dev id
func providerOverrideSection(provider string, cfg map[string]any) map[string]any {
	overrides := loadModelOverrides(cfg)
	key := strings.TrimSpace(provider)
	if len(overrides) == 0 || key == "" {
		return nil
	}
	candidates := []string{key}
	if mapped, ok := ModelsDevIDFor(key); ok {
		candidates = append(candidates, mapped)
	}
	candidates = append(candidates, modelsDevToHermesIDs(key)...)
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if section, ok := asMap(overrides[c]); ok {
			return section
		}
	}
	return nil
}

// Explicit per provider and model override, exact then case insensitive skipping _default
func explicitModelOverride(provider, model string, cfg map[string]any) map[string]any {
	key := strings.TrimSpace(model)
	if key == "" {
		return nil
	}
	section := providerOverrideSection(provider, cfg)
	if section == nil {
		return nil
	}
	if entry, ok := asMap(section[key]); ok {
		return entry
	}
	lower := strings.ToLower(key)
	for _, mid := range sortedMapKeys(section) {
		if mid == "_default" || strings.ToLower(mid) != lower {
			continue
		}
		if entry, ok := asMap(section[mid]); ok {
			return entry
		}
	}
	return nil
}

// Fill gap _default override, per provider first then global
func defaultModelOverride(provider string, cfg map[string]any) map[string]any {
	if section := providerOverrideSection(provider, cfg); section != nil {
		if d, ok := asMap(section["_default"]); ok {
			return d
		}
	}
	if d, ok := asMap(loadModelOverrides(cfg)["_default"]); ok {
		return d
	}
	return nil
}

// Explicit override if any, else the _default only on a catalog miss
func overrideFor(provider, model string, catalogHit bool, cfg map[string]any) map[string]any {
	if explicit := explicitModelOverride(provider, model, cfg); explicit != nil || catalogHit {
		return explicit
	}
	return defaultModelOverride(provider, cfg)
}

// Python int() over an override scalar, strings must be whole numbers
func pythonInt(raw any) (int, bool) {
	switch v := raw.(type) {
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case int:
		return v, true
	case int64:
		return int(v), true
	case int32:
		return int(v), true
	case float64:
		return int(v), true
	case float32:
		return int(v), true
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i), true
		}
		if f, err := v.Float64(); err == nil {
			return int(f), true
		}
		return 0, false
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(v))
		return i, err == nil
	}
	return 0, false
}

// Positive int override field, warning once per garbage value
func overrideInt(override map[string]any, key string) (int, bool) {
	raw, present := override[key]
	if !present || raw == nil {
		return 0, false
	}
	if n, ok := pythonInt(raw); ok && n > 0 {
		return n, true
	}
	warnKey := key + "\x00" + fmt.Sprintf("%#v", raw)
	modelsDevWarnMu.Lock()
	first := !modelsDevOverrideWarned[warnKey]
	modelsDevOverrideWarned[warnKey] = true
	modelsDevWarnMu.Unlock()
	if first {
		modelsDevLog.Warn(fmt.Sprintf("model_overrides: ignoring invalid %s value %#v (expected a positive integer)", key, raw))
	}
	return 0, false
}

// Explicitly overridden context window only, never the fill gap default
func overrideContextWindow(provider, model string, cfg map[string]any) (int, bool) {
	ov := explicitModelOverride(provider, model, cfg)
	if ov == nil {
		return 0, false
	}
	return overrideInt(ov, "context_window")
}

// Fill gap context from a _default override for catalog misses
func defaultOverrideContext(provider string, cfg map[string]any) (int, bool) {
	d := defaultModelOverride(provider, cfg)
	if d == nil {
		return 0, false
	}
	return overrideInt(d, "context_window")
}

// OverrideContextWindow is the explicit model_overrides context window for the bound home
func OverrideContextWindow(ctx context.Context, provider, model string) (int, bool) {
	return overrideContextWindow(provider, model, loadConfigTree(home.Dir(ctx)))
}

// Translates canonical override keys into a models.dev shaped patch plus an out of band vision flag
func overrideToCatalogShape(override map[string]any) (map[string]any, *bool) {
	patch := map[string]any{}
	if n, ok := overrideInt(override, "context_window"); ok {
		patch["limit"] = map[string]any{"context": float64(n)}
	}
	for _, pair := range [][2]string{{"supports_tools", "tool_call"}, {"supports_reasoning", "reasoning"}} {
		if v, ok := override[pair[0]]; ok {
			patch[pair[1]] = truthy(v)
		}
	}
	var vision *bool
	if v, ok := override["supports_vision"]; ok {
		b := truthy(v)
		vision = &b
		patch["attachment"] = b
	}
	if v, ok := override["model_family"]; ok {
		s := ""
		if truthy(v) {
			s = stringOf(v)
		}
		patch["family"] = s
	}
	return patch, vision
}

// Patches a catalog entry with a canonical override, merging limit and modalities instead of clobbering
func mergeCatalogEntryWithOverride(raw, override map[string]any) map[string]any {
	shaped, vision := overrideToCatalogShape(override)
	merged := make(map[string]any, len(raw)+len(shaped))
	for k, v := range raw {
		merged[k] = v
	}
	if limitPatch, ok := asMap(shaped["limit"]); ok && len(limitPatch) > 0 {
		limit := map[string]any{}
		for k, v := range dictOrEmpty(raw["limit"]) {
			limit[k] = v
		}
		for k, v := range limitPatch {
			limit[k] = v
		}
		merged["limit"] = limit
	}
	delete(shaped, "limit")
	if vision != nil {
		mods := map[string]any{}
		for k, v := range dictOrEmpty(raw["modalities"]) {
			mods[k] = v
		}
		var input []any
		if list, ok := asList(mods["input"]); ok {
			input = append([]any(nil), list...)
		} else if strs, ok := mods["input"].([]string); ok {
			for _, s := range strs {
				input = append(input, s)
			}
		} else {
			input = []any{}
		}
		hasImage := false
		for _, m := range input {
			if m == "image" {
				hasImage = true
				break
			}
		}
		if *vision && !hasImage {
			input = append(input, "image")
		} else if !*vision && hasImage {
			var kept []any
			removed := false
			for _, m := range input {
				if !removed && m == "image" {
					removed = true
					continue
				}
				kept = append(kept, m)
			}
			input = kept
			if input == nil {
				input = []any{}
			}
		}
		mods["input"] = input
		merged["modalities"] = mods
	}
	for k, v := range shaped {
		merged[k] = v
	}
	return merged
}

// Built in metadata for a provider and model pair when a vendor specific entry exists
func builtinModelMetadata(h, provider, model string, cfg map[string]any) map[string]any {
	key := modelsDevID(h, provider, cfg)
	if key == "" {
		key = strings.TrimSpace(provider)
	}
	return builtinModelMetadataTable()[[2]string{key, strings.ToLower(strings.TrimSpace(model))}]
}

// Fill gap base for an OpenCode relay vision marked id the catalog does not know
func relayVisionMarkerMetadata(provider, model string) map[string]any {
	if !strings.Contains(strings.ToLower(strings.TrimSpace(model)), "-vision") || OpenCodeFamily(provider) == "" {
		return nil
	}
	base := unknownModelBase()
	base["modalities"] = map[string]any{"input": []any{"text", "image"}, "output": []any{"text"}}
	return base
}

// RegisterModelCapabilities records a plugin profile's exact model declarations in the canonical override schema
func RegisterModelCapabilities(provider string, caps map[string]map[string]any) {
	modelCapabilitiesMu.Lock()
	defer modelCapabilitiesMu.Unlock()
	key := NormalizeProvider(provider)
	if caps == nil {
		delete(registeredModelCaps, key)
		return
	}
	copied := make(map[string]map[string]any, len(caps))
	for model, decl := range caps {
		entry := make(map[string]any, len(decl))
		for k, v := range decl {
			entry[k] = v
		}
		copied[model] = entry
	}
	registeredModelCaps[key] = copied
}

// Exact model declaration from the registered profile capabilities, the one plugin seam
func providerModelCapabilities(provider, model string) map[string]any {
	modelCapabilitiesMu.Lock()
	defer modelCapabilitiesMu.Unlock()
	caps := registeredModelCaps[NormalizeProvider(provider)]
	if caps == nil {
		return map[string]any{}
	}
	decl := caps[model]
	if decl == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(decl))
	for k, v := range decl {
		out[k] = v
	}
	return out
}

// Catalog or builtin metadata patched by the plugin declaration then the user override
func applyModelsDevOverrides(h, provider, model string, entry map[string]any, cfg map[string]any) map[string]any {
	base := entry
	if base == nil {
		base = builtinModelMetadata(h, provider, model, cfg)
	}
	if declared := providerModelCapabilities(provider, model); len(declared) > 0 {
		if base == nil {
			base = unknownModelBase()
		}
		base = mergeCatalogEntryWithOverride(base, declared)
	}
	override := overrideFor(provider, model, base != nil, cfg)
	if base == nil {
		base = relayVisionMarkerMetadata(provider, model)
	}
	if override == nil {
		return base
	}
	if base == nil {
		base = unknownModelBase()
	}
	return mergeCatalogEntryWithOverride(base, override)
}

// Vision from explicit input modalities with the attachment flag only as a fallback
func entrySupportsVision(entry map[string]any) bool {
	if list, ok := asList(dictOrEmpty(entry["modalities"])["input"]); ok {
		for _, m := range list {
			if m == "image" {
				return true
			}
		}
		return false
	}
	if strs, ok := dictOrEmpty(entry["modalities"])["input"].([]string); ok {
		return containsString(strs, "image")
	}
	return truthy(entry["attachment"])
}

// ModelsDevCapabilities returns the capability view with overrides applied, false when nothing knows the model
func ModelsDevCapabilities(ctx context.Context, provider, model string, allowNetwork bool) (ModelCapabilities, bool) {
	h := home.Dir(ctx)
	cfg := loadConfigTree(h)
	return modelsDevCapabilitiesIn(h, provider, model, allowNetwork, cfg)
}

func modelsDevCapabilitiesIn(h, provider, model string, allowNetwork bool, cfg map[string]any) (ModelCapabilities, bool) {
	models := modelsDevProviderModels(h, provider, allowNetwork, cfg)
	var entry map[string]any
	if models != nil {
		entry = findModelsDevEntry(models, model, provider)
	}
	unknownBase := entry == nil && builtinModelMetadata(h, provider, model, cfg) == nil
	raw := applyModelsDevOverrides(h, provider, model, entry, cfg)
	if raw == nil {
		return ModelCapabilities{}, false
	}
	caps := ModelCapabilities{SupportsTools: truthy(raw["tool_call"]), ContextWindow: extractModelsDevLimit(raw, "context"), MaxOutputTokens: extractModelsDevLimit(raw, "output"), ModelFamily: stringOf(raw["family"])}
	if caps.ContextWindow == 0 {
		caps.ContextWindow = 200000
	}
	_, hasAttachment := raw["attachment"]
	_, hasModalities := raw["modalities"]
	if !(unknownBase && !hasAttachment && !hasModalities) {
		v := entrySupportsVision(raw)
		caps.SupportsVision = &v
	}
	if _, hasReasoning := raw["reasoning"]; !(unknownBase && !hasReasoning) {
		r := truthy(raw["reasoning"])
		caps.SupportsReasoning = &r
	}
	return caps, true
}

// Normalizes a picker alias to its canonical provider id with openrouter as the empty default
func catalogNormalizeProvider(provider string) string {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if normalized == "" {
		normalized = "openrouter"
	}
	if target, ok := CatalogProviderAlias(normalized); ok {
		return target
	}
	return normalized
}

// ListModelsDevModels returns every model id of a provider minus the hidden Google rows
func ListModelsDevModels(ctx context.Context, provider string, allowNetwork bool) []string {
	h := home.Dir(ctx)
	normalized := catalogNormalizeProvider(provider)
	if normalized == "" {
		normalized = provider
	}
	models := modelsDevProviderModels(h, normalized, allowNetwork, loadConfigTree(h))
	if models == nil {
		return []string{}
	}
	out := []string{}
	for _, mid := range sortedMapKeys(models) {
		if !shouldHideFromProviderCatalog(normalized, mid) {
			out = append(out, mid)
		}
	}
	return out
}

// Non agentic or noise ids such as tts, embedding, dated previews and image only models
var modelsDevNoisePattern = regexp.MustCompile(`(?i)-tts\b|embedding|live-|-(preview|exp)-\d{2,4}[-_]|-image\b|-image-preview\b|-customtools\b`)

var googleHiddenModels = map[string]bool{
	"gemma-4-31b-it": true, "gemma-4-26b-it": true, "gemma-4-26b-a4b-it": true,
	"gemma-3-1b": true, "gemma-3-1b-it": true, "gemma-3-2b": true, "gemma-3-2b-it": true,
	"gemma-3-4b": true, "gemma-3-4b-it": true, "gemma-3-12b": true, "gemma-3-12b-it": true,
	"gemma-3-27b": true, "gemma-3-27b-it": true,
	"gemini-1.5-flash": true, "gemini-1.5-pro": true, "gemini-1.5-flash-8b": true,
	"gemini-2.0-flash": true, "gemini-2.0-flash-lite": true,
}

func shouldHideFromProviderCatalog(provider, modelID string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	return (p == "gemini" || p == "google") && googleHiddenModels[strings.ToLower(strings.TrimSpace(modelID))]
}

// ListAgenticModels returns tool calling model ids minus hidden and noise entries
func ListAgenticModels(ctx context.Context, provider string, allowNetwork bool) []string {
	h := home.Dir(ctx)
	models := modelsDevProviderModels(h, provider, allowNetwork, loadConfigTree(h))
	if models == nil {
		return []string{}
	}
	out := []string{}
	for _, mid := range sortedMapKeys(models) {
		entry, ok := asMap(models[mid])
		if !ok || shouldHideFromProviderCatalog(provider, mid) || !truthy(entry["tool_call"]) || modelsDevNoisePattern.MatchString(mid) {
			continue
		}
		out = append(out, mid)
	}
	return out
}

func stringList(v any) []string {
	if list, ok := asList(v); ok {
		out := make([]string, 0, len(list))
		for _, item := range list {
			out = append(out, stringOf(item))
		}
		return out
	}
	if strs, ok := v.([]string); ok {
		return cloneStrings(strs)
	}
	return nil
}

func floatOrZero(v any) float64 {
	if v == nil {
		return 0
	}
	f, _ := floatOf(v)
	return f
}

// Converts a raw model row into a ModelsDevEntry
func parseModelsDevEntry(modelID string, raw map[string]any, providerID string) ModelsDevEntry {
	cost := dictOrEmpty(raw["cost"])
	mods := dictOrEmpty(raw["modalities"])
	optCost := func(key string) *float64 {
		if v, ok := cost[key]; ok && v != nil {
			f, _ := floatOf(v)
			return &f
		}
		return nil
	}
	name := stringOf(raw["name"])
	if name == "" {
		name = modelID
	}
	entry := ModelsDevEntry{
		ID: modelID, Name: name, Family: stringOf(raw["family"]), ProviderID: providerID,
		Reasoning: truthy(raw["reasoning"]), ToolCall: truthy(raw["tool_call"]), Attachment: truthy(raw["attachment"]),
		Temperature: truthy(raw["temperature"]), StructuredOutput: truthy(raw["structured_output"]), OpenWeights: truthy(raw["open_weights"]),
		InputModalities: stringList(mods["input"]), OutputModalities: stringList(mods["output"]),
		ContextWindow: extractModelsDevLimit(raw, "context"), MaxOutput: extractModelsDevLimit(raw, "output"), MaxInput: extractModelsDevLimit(raw, "input"),
		CostInput: floatOrZero(cost["input"]), CostOutput: floatOrZero(cost["output"]),
		CostCacheRead: optCost("cache_read"), CostCacheWrite: optCost("cache_write"),
		KnowledgeCutoff: stringOf(raw["knowledge"]), ReleaseDate: stringOf(raw["release_date"]), Status: stringOf(raw["status"]),
		Interleaved: raw["interleaved"], Raw: raw,
	}
	if entry.Interleaved == nil {
		entry.Interleaved = false
	}
	return entry
}

// Converts a raw provider row into a ModelsDevProvider
func parseModelsDevProvider(providerID string, raw map[string]any) ModelsDevProvider {
	name := stringOf(raw["name"])
	if name == "" {
		name = providerID
	}
	env := stringList(raw["env"])
	if env == nil {
		env = []string{}
	}
	return ModelsDevProvider{ID: providerID, Name: name, Env: env, API: stringOf(raw["api"]), Doc: stringOf(raw["doc"]), ModelCount: len(dictOrEmpty(raw["models"]))}
}

// ModelsDevProviderInfo returns provider metadata by hermes or models.dev id
func ModelsDevProviderInfo(ctx context.Context, providerID string, allowNetwork bool) (ModelsDevProvider, bool) {
	h := home.Dir(ctx)
	return modelsDevProviderInfoIn(h, providerID, allowNetwork, loadConfigTree(h))
}

func modelsDevProviderInfoIn(h, providerID string, allowNetwork bool, cfg map[string]any) (ModelsDevProvider, bool) {
	mdevID := modelsDevID(h, providerID, cfg)
	if mdevID == "" {
		mdevID = providerID
	}
	raw := modelsDevRegistryProvider(h, mdevID, allowNetwork)
	if raw == nil {
		return ModelsDevProvider{}, false
	}
	return parseModelsDevProvider(mdevID, raw), true
}

// ModelsDevModelInfo returns full model metadata with overrides applied, exact then case insensitive
func ModelsDevModelInfo(ctx context.Context, providerID, modelID string, allowNetwork bool) (ModelsDevEntry, bool) {
	h := home.Dir(ctx)
	return modelsDevModelInfoIn(h, providerID, modelID, allowNetwork, loadConfigTree(h))
}

func modelsDevModelInfoIn(h, providerID, modelID string, allowNetwork bool, cfg map[string]any) (ModelsDevEntry, bool) {
	mdevID := modelsDevID(h, providerID, cfg)
	if mdevID == "" {
		mdevID = providerID
	}
	models := modelsDevRegistryModels(h, mdevID, allowNetwork)
	mid := modelID
	var entry map[string]any
	if models != nil {
		for _, m := range iterModelsDevEntries(models, modelID, false, providerID) {
			mid, entry = m.id, m.entry
			break
		}
	}
	raw := applyModelsDevOverrides(h, providerID, modelID, entry, cfg)
	if raw == nil {
		return ModelsDevEntry{}, false
	}
	return parseModelsDevEntry(mid, raw, mdevID), true
}

// ModelsDevProfile is the hot path model lookup that never touches the network
func ModelsDevProfile(ctx context.Context, provider, model string) (ModelsDevEntry, bool) {
	return ModelsDevModelInfo(ctx, provider, model, false)
}

// Provider definition synthesized from the offline registry for a provider without a built in profile
func modelsDevProfileOffline(name string) (Profile, bool) {
	canonical := NormalizeProvider(name)
	if canonical == "" || ProviderLeftCore(canonical) {
		return Profile{}, false
	}
	h := home.Process()
	info, ok := modelsDevProviderInfoIn(h, canonical, false, loadConfigTree(h))
	if !ok {
		return Profile{}, false
	}
	return Profile{
		Name: canonical, DisplayName: info.Name, SignupURL: info.Doc, BaseURL: info.API,
		Mode: ModeChatCompletions, Auth: AuthAPIKey, AuthType: AuthTypeAPIKey,
		KeyEnv: cloneStrings(info.Env), ModelsDevID: info.ID, ModelListing: true, HealthCheck: true,
	}, true
}

// Context window from the offline registry with overrides applied, zero when unknown
func lookupModelsDevContext(ctx context.Context, provider, model string) int {
	n, _ := LookupModelsDevContext(ctx, provider, model, false)
	return n
}

// Explicit model_overrides context window for the bound home
func modelsDevOverrideContextWindow(ctx context.Context, provider, model string) (int, bool) {
	return OverrideContextWindow(ctx, provider, model)
}
