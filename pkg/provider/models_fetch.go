package provider

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Clock behind every catalog TTL in this slice
var modelsClock = time.Now

// Transport seam so catalog tests never open a socket
var modelsTransport http.RoundTripper

// Non 2xx catalog response
type modelsHTTPError struct {
	URL    string
	Status int
	Body   []byte
}

func (e *modelsHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d for %s", e.Status, e.URL)
}

// GET a catalog URL with credentials scoped to its origin and parse its JSON body
func modelsGetJSON(ctx context.Context, rawURL string, timeout time.Duration, headers map[string]string, tlsCfg *tls.Config) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	h := make(http.Header, len(headers))
	for _, k := range slices.Sorted(maps.Keys(headers)) {
		h.Set(k, headers[k])
	}
	acceptEncoding := h.Get("Accept-Encoding")
	resp, err := urllibOpen(ctx, urllibCall{url: rawURL, headers: h, credentialed: true, tls: tlsCfg, timeout: timeout, base: modelsTransport})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &modelsHTTPError{URL: rawURL, Status: resp.StatusCode, Body: body}
	}
	if acceptEncoding == "gzip" && strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		body, err = io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
	}
	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// True when the error is a connect or read timeout rather than a refusal or protocol failure
func modelsErrIsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// JSON object cache file, nil when missing, unreadable or not an object
func modelsReadJSONCache(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	m, _ := asMap(out)
	return m
}

// Atomically persists a cache file with the dump options, creating parents unless they lie in a dead profile
func modelsWriteJSONCache(ctx context.Context, path string, data any, opts ...jsonutil.Option) error {
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	return config.AtomicJSONWriteWith(ctx, path, data, opts...)
}

// Primary verbatim then the secondary entries whose key is new
func mergeUnique(primary, secondary []string, key func(string) string) []string {
	if key == nil {
		key = strings.ToLower
	}
	merged := append([]string{}, primary...)
	seen := map[string]bool{}
	for _, m := range primary {
		seen[key(m)] = true
	}
	for _, m := range secondary {
		k := key(m)
		if !seen[k] {
			seen[k] = true
			merged = append(merged, m)
		}
	}
	return merged
}

// ---- OpenRouter curated list, process slot plus disk mirror ----

var openrouterCatalogSlot = newProfileSlot[[]CatalogModel]()

// Same TTL as the catalog manifest the list is filtered from
func openrouterCatalogDiskTTL(ctx context.Context) time.Duration { return CatalogRefreshInterval(ctx) }

func openrouterCatalogDiskPath(ctx context.Context) string {
	return filepath.Join(home.CacheDir(home.Dir(ctx)), "openrouter_curated_catalog.json")
}

// Fresh curated catalog from disk, nil when missing, corrupt, expired or empty
func readOpenrouterCatalogDisk(ctx context.Context, allowStale bool) []CatalogModel {
	obj := modelsReadJSONCache(openrouterCatalogDiskPath(ctx))
	if obj == nil {
		return nil
	}
	if !allowStale {
		fetchedAt, ok := floatOf(obj["fetched_at"])
		if !ok {
			return nil
		}
		if float64(modelsClock().Unix())-fetchedAt > openrouterCatalogDiskTTL(ctx).Seconds() {
			return nil
		}
	}
	items, ok := asList(obj["curated"])
	if !ok {
		return nil
	}
	var out []CatalogModel
	for _, it := range items {
		pair, ok := asList(it)
		if !ok || len(pair) != 2 {
			continue
		}
		out = append(out, CatalogModel{ID: stringOf(pair[0]), Description: stringOf(pair[1])})
	}
	return out
}

func writeOpenrouterCatalogDisk(ctx context.Context, curated []CatalogModel) {
	rows := make([][]string, 0, len(curated))
	for _, c := range curated {
		rows = append(rows, []string{c.ID, c.Description})
	}
	if err := modelsWriteJSONCache(ctx, openrouterCatalogDiskPath(ctx), jsonutil.ObjectOf("fetched_at", float64(modelsClock().UnixNano())/1e9, "curated", rows), jsonutil.Indent(2)); err != nil {
		pkgLog().Debug("openrouter curated catalog disk write failed", "error", err)
	}
}

// ---- OpenRouter and AI Gateway live catalogs ----

func openrouterModelIsFree(pricing any) bool {
	return zeroPriced(pricing, [2]string{"prompt", "completion"}, "0")
}

// True when supported_parameters advertises tools, permissive when the field is absent or malformed
func openrouterModelSupportsTools(item any) bool {
	m, ok := asMap(item)
	if !ok {
		return true
	}
	params, ok := asList(m["supported_parameters"])
	if !ok {
		return true
	}
	for _, p := range params {
		if p == "tools" {
			return true
		}
	}
	return false
}

// ClampReasoningEffortToSupported keeps a supported level verbatim else the nearest weaker supported level
func ClampReasoningEffortToSupported(effort string, supported []string) string {
	return clampEffort(effort, supported, nil)
}

// GET an OpenAI style models listing as the raw data array and an id index
func fetchLiveCatalogIndex(ctx context.Context, rawURL string, timeout time.Duration) ([]any, map[string]map[string]any, bool) {
	payload, err := modelsGetJSON(ctx, rawURL, timeout, map[string]string{"Accept": "application/json"}, nil)
	if err != nil {
		return nil, nil, false
	}
	doc, ok := asMap(payload)
	if !ok {
		return nil, nil, false
	}
	items, ok := asList(doc["data"])
	if !ok {
		return nil, nil, false
	}
	byID := map[string]map[string]any{}
	for _, it := range items {
		m, ok := asMap(it)
		if !ok {
			continue
		}
		if mid := strings.TrimSpace(stringOf(m["id"])); mid != "" {
			byID[mid] = m
		}
	}
	return items, byID, true
}

// FetchModelsOptions tune one picker list fetch
type FetchModelsOptions struct {
	Timeout      time.Duration
	ForceRefresh bool
	CacheOnly    bool
}

func cloneCatalogModels(rows []CatalogModel) []CatalogModel {
	return append([]CatalogModel{}, rows...)
}

// FetchOpenRouterModels returns the curated OpenRouter picker list refreshed from the live catalog when possible
func FetchOpenRouterModels(ctx context.Context, opts FetchModelsOptions) []CatalogModel {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	cached, haveCached := openrouterCatalogSlot.get(ctx)
	if haveCached && !opts.ForceRefresh {
		return cloneCatalogModels(cached)
	}
	if !opts.ForceRefresh {
		if disk := readOpenrouterCatalogDisk(ctx, opts.CacheOnly); len(disk) > 0 {
			if !opts.CacheOnly {
				openrouterCatalogSlot.set(ctx, disk)
			}
			return cloneCatalogModels(disk)
		}
	}
	if opts.CacheOnly {
		return cloneCatalogModels(OpenRouterModels)
	}
	fallback := curatedOpenRouterModelsFn(ctx)
	if len(fallback) == 0 {
		fallback = cloneCatalogModels(OpenRouterModels)
	}
	liveItems, liveByID, ok := fetchLiveCatalogIndexFn(ctx, openRouterCatalogURL, timeout)
	if !ok {
		if haveCached {
			return cloneCatalogModels(cached)
		}
		return cloneCatalogModels(fallback)
	}
	seedReasoningCaps(ctx, openRouterCatalogURL, liveItems)
	var curated []CatalogModel
	silentDefault := preferredSilentDefaultFn(ctx, "openrouter")
	for _, row := range fallback {
		liveItem, ok := liveByID[row.ID]
		if !ok || !openrouterModelSupportsTools(liveItem) {
			continue
		}
		desc := ""
		if row.ID == silentDefault {
			desc = "default"
		} else if openrouterModelIsFree(liveItem["pricing"]) {
			desc = "free"
		}
		curated = append(curated, CatalogModel{ID: row.ID, Description: desc})
	}
	if len(curated) == 0 {
		if haveCached {
			return cloneCatalogModels(cached)
		}
		return cloneCatalogModels(fallback)
	}
	if curated[0].Description == "" {
		curated[0].Description = "recommended"
	}
	openrouterCatalogSlot.set(ctx, curated)
	writeOpenrouterCatalogDisk(ctx, curated)
	return cloneCatalogModels(curated)
}

// Seam form the catalog refresh command drives
func fetchOpenRouterModels(ctx context.Context, forceRefresh bool) []CatalogModel {
	return FetchOpenRouterModels(ctx, FetchModelsOptions{ForceRefresh: forceRefresh})
}

// OpenRouterModelIDs returns just the OpenRouter model id strings
func OpenRouterModelIDs(ctx context.Context, forceRefresh bool) []string {
	return CatalogModelIDs(FetchOpenRouterModels(ctx, FetchModelsOptions{ForceRefresh: forceRefresh}))
}

func aiGatewayModelIsFree(pricing any) bool {
	return zeroPriced(pricing, [2]string{"input", "output"}, "0")
}

var (
	aiGatewayCatalogMu    sync.Mutex
	aiGatewayCatalogCache []CatalogModel
)

// FetchAIGatewayModels returns the curated AI Gateway picker list refreshed from the live catalog when possible
func FetchAIGatewayModels(ctx context.Context, timeout time.Duration, forceRefresh bool) []CatalogModel {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	aiGatewayCatalogMu.Lock()
	cached := aiGatewayCatalogCache
	aiGatewayCatalogMu.Unlock()
	if cached != nil && !forceRefresh {
		return cloneCatalogModels(cached)
	}
	fallback := cloneCatalogModels(VercelAIGatewayModels)
	_, liveByID, ok := fetchLiveCatalogIndex(ctx, strings.TrimRight(AIGatewayBaseURL, "/")+"/models", timeout)
	if !ok {
		if cached != nil {
			return cloneCatalogModels(cached)
		}
		return fallback
	}
	var curated []CatalogModel
	for _, row := range fallback {
		item, ok := liveByID[row.ID]
		if !ok {
			continue
		}
		desc := ""
		if aiGatewayModelIsFree(item["pricing"]) {
			desc = "free"
		}
		curated = append(curated, CatalogModel{ID: row.ID, Description: desc})
	}
	if len(curated) == 0 {
		if cached != nil {
			return cloneCatalogModels(cached)
		}
		return fallback
	}
	freeMoonshot := ""
	for _, mid := range sortedStringKeys(liveByID) {
		if strings.HasPrefix(mid, "moonshotai/") && aiGatewayModelIsFree(liveByID[mid]["pricing"]) {
			freeMoonshot = mid
			break
		}
	}
	if freeMoonshot != "" {
		promoted := []CatalogModel{{ID: freeMoonshot, Description: "recommended"}}
		for _, row := range curated {
			if row.ID != freeMoonshot {
				promoted = append(promoted, row)
			}
		}
		curated = promoted
	} else {
		curated[0].Description = "recommended"
	}
	aiGatewayCatalogMu.Lock()
	aiGatewayCatalogCache = curated
	aiGatewayCatalogMu.Unlock()
	return cloneCatalogModels(curated)
}

// AIGatewayModelIDs returns just the AI Gateway model id strings
func AIGatewayModelIDs(ctx context.Context, forceRefresh bool) []string {
	return CatalogModelIDs(FetchAIGatewayModels(ctx, 0, forceRefresh))
}

// Live AI Gateway language models with tool use, false without a key or on failure
func fetchAIGatewayModelsLive(ctx context.Context, timeout time.Duration) ([]string, bool) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	apiKey := strings.TrimSpace(GetSecretStr(ctx, "AI_GATEWAY_API_KEY", ""))
	if apiKey == "" {
		return nil, false
	}
	baseURL := strings.TrimSpace(GetSecretStr(ctx, "AI_GATEWAY_BASE_URL", ""))
	if baseURL == "" {
		baseURL = AIGatewayBaseURL
	}
	headers := map[string]string{"Authorization": "Bearer " + apiKey, "User-Agent": userAgent("icebath-cli")}
	payload, err := modelsGetJSON(ctx, strings.TrimRight(baseURL, "/")+"/models", timeout, headers, nil)
	if err != nil {
		return nil, false
	}
	doc, ok := asMap(payload)
	if !ok {
		return nil, false
	}
	items, _ := asList(doc["data"])
	out := []string{}
	for _, it := range items {
		m, ok := asMap(it)
		if !ok || stringOf(m["id"]) == "" || m["type"] != "language" {
			continue
		}
		tags, _ := asList(m["tags"])
		for _, t := range tags {
			if t == "tool-use" {
				out = append(out, stringOf(m["id"]))
				break
			}
		}
	}
	return out, true
}

// ---- DeepInfra tagged catalog ----

var deepinfraExcludeRe = regexp.MustCompile(`(?i)(embed|rerank|whisper|stable-diffusion|flux|sdxl|tts|bark|speech|image-gen|clip|vit-|dpt-)`)

var deepinfraSurfaceTags = map[string]bool{"chat": true, "embed": true, "image-gen": true, "tts": true, "stt": true, "video-gen": true}

const (
	// DeepinfraDefaultBaseURL is the OpenAI compatible DeepInfra root
	DeepinfraDefaultBaseURL = "https://api.deepinfra.com/v1/openai"
	deepinfraModelsQuery    = "filter=true&sort_by=icebath"
	deepinfraCatalogNegTTL  = 60 * time.Second
)

var (
	deepinfraCatalogMu       sync.Mutex
	deepinfraCatalogCache    = map[string][]map[string]any{}
	deepinfraCatalogNegCache = map[string]time.Time{}
)

// Pool environment for one profile home so credential reads follow the profile .env and scope
func modelsPoolEnv(ctx context.Context) PoolEnv {
	if ctx == nil {
		ctx = context.Background()
	}
	h := home.Dir(ctx)
	return PoolEnv{Store: NewStore(h), Secrets: config.NewSecrets(h), Scope: SecretScopeFrom(ctx), Log: pkgLog(), Ctx: ctx}
}

// Profile scoped .env then environ read of a DeepInfra setting
func deepinfraEnv(ctx context.Context, key string) string {
	return strings.TrimSpace(GetEnvPreferDotenv(modelsPoolEnv(ctx), key))
}

// Cache key carrying the api key fingerprint plus the full catalog URL
func deepinfraCatalogURLIn(ctx context.Context) (string, string) {
	base := deepinfraEnv(ctx, "DEEPINFRA_BASE_URL")
	if base == "" {
		base = DeepinfraDefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	fp := secretFingerprintOf(deepinfraEnv(ctx, "DEEPINFRA_API_KEY"))
	if fp == "" {
		fp = "anon"
	}
	return base + "#" + fp, base + "/models?" + deepinfraModelsQuery
}

// Seam form the pricing scope reads, resolved against the process home
func deepinfraCatalogURL() (string, string) { return deepinfraCatalogURLIn(context.Background()) }

// True when the tagged catalog for a cache key is resident in this process
func deepinfraCatalogCached(cacheKey string) bool {
	deepinfraCatalogMu.Lock()
	defer deepinfraCatalogMu.Unlock()
	_, ok := deepinfraCatalogCache[cacheKey]
	return ok
}

// Raw DeepInfra catalog list cached per base URL and key, false on failure
func fetchDeepinfraCatalog(ctx context.Context, timeout time.Duration, forceRefresh bool) ([]map[string]any, bool) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cacheKey, rawURL := deepinfraCatalogURLIn(ctx)
	if !forceRefresh {
		deepinfraCatalogMu.Lock()
		cached, ok := deepinfraCatalogCache[cacheKey]
		lastFail, failed := deepinfraCatalogNegCache[cacheKey]
		deepinfraCatalogMu.Unlock()
		if ok {
			return cached, true
		}
		if failed && modelsClock().Sub(lastFail) < deepinfraCatalogNegTTL {
			return nil, false
		}
	}
	headers := map[string]string{"User-Agent": userAgent("icebath-cli")}
	if apiKey := deepinfraEnv(ctx, "DEEPINFRA_API_KEY"); apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	payload, err := modelsGetJSON(ctx, rawURL, timeout, headers, nil)
	doc, _ := asMap(payload)
	items, ok := asList(doc["data"])
	if err != nil || !ok {
		deepinfraCatalogMu.Lock()
		deepinfraCatalogNegCache[cacheKey] = modelsClock()
		deepinfraCatalogMu.Unlock()
		return nil, false
	}
	data := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := asMap(it); ok {
			data = append(data, m)
		}
	}
	deepinfraCatalogMu.Lock()
	deepinfraCatalogCache[cacheKey] = data
	delete(deepinfraCatalogNegCache, cacheKey)
	deepinfraCatalogMu.Unlock()
	return data, true
}

// DeepInfra id and metadata items whose surface tags include the tag, untagged ids fall to the regex
func fetchDeepinfraModelsByTagIn(ctx context.Context, tag string, timeout time.Duration, forceRefresh bool) ([]map[string]any, bool) {
	data, ok := fetchDeepinfraCatalog(ctx, timeout, forceRefresh)
	if !ok {
		return nil, false
	}
	matched := []map[string]any{}
	for _, item := range data {
		mid := stringOf(item["id"])
		rawMetadata, present := item["metadata"]
		if mid == "" || !present || rawMetadata == nil {
			continue
		}
		metadata, _ := asMap(rawMetadata)
		if metadata == nil {
			metadata = map[string]any{}
		}
		tags, _ := asList(metadata["tags"])
		surface, hit := false, false
		for _, t := range tags {
			if s, ok := t.(string); ok && deepinfraSurfaceTags[s] {
				surface = true
			}
			if t == tag {
				hit = true
			}
		}
		if !surface {
			hit = tag == "chat" && !deepinfraExcludeRe.MatchString(mid)
		}
		if hit {
			matched = append(matched, map[string]any{"id": mid, "metadata": metadata})
		}
	}
	return matched, true
}

// Seam form the pricing fetcher drives, nil on network failure
func fetchDeepinfraModelsByTag(tag string, timeout time.Duration, forceRefresh bool) []map[string]any {
	items, ok := fetchDeepinfraModelsByTagIn(context.Background(), tag, timeout, forceRefresh)
	if !ok {
		return nil
	}
	return items
}

// DeepInfra chat model ids, false on failure or when no chat id exists
func fetchDeepinfraModels(ctx context.Context, timeout time.Duration, forceRefresh bool) ([]string, bool) {
	items, ok := fetchDeepinfraModelsByTagIn(ctx, "chat", timeout, forceRefresh)
	if !ok {
		return nil, false
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, stringOf(it["id"]))
	}
	return ids, len(ids) > 0
}

// DeepinfraModelIDs returns the DeepInfra ids carrying a surface tag, empty on failure
func DeepinfraModelIDs(ctx context.Context, tag string, forceRefresh bool) []string {
	items, ok := fetchDeepinfraModelsByTagIn(ctx, tag, 5*time.Second, forceRefresh)
	if !ok {
		return []string{}
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, stringOf(it["id"]))
	}
	return ids
}

// DeepinfraBaseURL is the config section base_url, then the env override, then the default
func DeepinfraBaseURL(ctx context.Context, section map[string]any) string {
	value := strings.TrimSpace(stringOf(section["base_url"]))
	if value == "" {
		value = deepinfraEnv(ctx, "DEEPINFRA_BASE_URL")
	}
	if value == "" {
		value = DeepinfraDefaultBaseURL
	}
	return strings.TrimRight(strings.TrimSpace(value), "/")
}
