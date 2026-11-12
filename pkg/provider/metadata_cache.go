package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// In memory TTL of the OpenRouter catalog
const modelCacheTTL = 3600 * time.Second

// In memory and disk TTL of a remote models probe
const endpointModelCacheTTL = 300 * time.Second

// TTL of a positive server type verdict
const endpointProbeTTL = 3600 * time.Second

// TTL of a failed server type verdict so a transient failure recovers in minutes
const endpointProbeFailureTTL = 300 * time.Second

// Window during which later probes to a connect timed out host short circuit
var endpointBlackholeTTL = 30 * time.Second

// Disk TTL of successful local endpoint probes
const localProbeDiskTTL = 300 * time.Second

// Bound on the per credential codex context memos
const codexCacheMax = 128

// Cross process lock wait for the context length cache file
var contextCacheLockTimeout = 5 * time.Second

var metadataLog = slog.Default().With("logger", "agent.model_metadata")

// Every writer of a paired memo holds this from sweep to publish
var memoLock sync.Mutex

var (
	modelMetadataCache     map[string]map[string]any
	modelMetadataCacheTime time.Time
)

type endpointMemoKey struct{ url, fingerprint string }

var (
	endpointModelMetadataCache     = map[endpointMemoKey]map[string]map[string]any{}
	endpointModelMetadataCacheTime = map[endpointMemoKey]time.Time{}
)

type serverTypeVerdict struct {
	serverType string
	known      bool
	at         time.Time
}

var (
	endpointProbePathMu    sync.Mutex
	endpointProbePathCache = map[string]serverTypeVerdict{}
)

var (
	endpointBlackholeMu    sync.Mutex
	endpointBlackholeCache = map[string]time.Time{}
)

var monotonicNow = time.Now

// Wall clock used for every TTL stamp so tests can age memos
var wallNow = time.Now

// Home directory the metadata caches live under
func metadataHome(ctx context.Context) string { return home.Dir(ctx) }

func metadataCacheFile(ctx context.Context, name string) string {
	return filepath.Join(home.CacheDir(metadataHome(ctx)), name)
}

// Effective config tree of the bound home, nil when it cannot be read
func metadataConfig(ctx context.Context) map[string]any {
	tree, err := config.Load(metadataHome(ctx), nil)
	if err != nil {
		return nil
	}
	return map[string]any(tree)
}

// Host and port key so every probe path for one server shares an entry
func endpointHostKey(baseURL string) string {
	parsed := parseProbeURL(baseURL, "http")
	if parsed == nil || parsed.Hostname() == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
	}
	return parsed.Hostname() + ":" + port
}

func noteEndpointBlackholed(baseURL string) {
	key := endpointHostKey(baseURL)
	if key == "" {
		return
	}
	endpointBlackholeMu.Lock()
	endpointBlackholeCache[key] = monotonicNow()
	endpointBlackholeMu.Unlock()
	metadataLog.Debug("endpoint timed out connecting, skipping further probes", "endpoint", key, "seconds", endpointBlackholeTTL.Seconds())
}

// Reports whether a recent probe to the host timed out during connect
func endpointBlackholed(baseURL string) bool {
	if endpointBlackholeTTL <= 0 {
		return false
	}
	key := endpointHostKey(baseURL)
	if key == "" {
		return false
	}
	endpointBlackholeMu.Lock()
	defer endpointBlackholeMu.Unlock()
	seen, ok := endpointBlackholeCache[key]
	if !ok {
		return false
	}
	if monotonicNow().Sub(seen) >= endpointBlackholeTTL {
		delete(endpointBlackholeCache, key)
		return false
	}
	return true
}

func noteIfConnectTimeout(err error, baseURL string) {
	if isConnectTimeout(err) {
		noteEndpointBlackholed(baseURL)
	}
}

// JSON object at the path or an empty map when missing or invalid
func loadJSONDict(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var data any
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &data); err != nil {
		return map[string]any{}
	}
	m, ok := data.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

func atomicJSONWrite(path string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return config.AtomicWrite(path, raw, 0o600)
}

// Fresh value of a key in a TTL memo file, nil when missing or expired
func ttlMemoGet(path, key string, ttl time.Duration, tsKey, valueKey string) any {
	entry, ok := loadJSONDict(path)[key].(map[string]any)
	if !ok {
		return nil
	}
	stamp, ok := config.AsFloat(entry[tsKey])
	if !ok {
		return nil
	}
	if float64(wallNow().Unix())-stamp >= ttl.Seconds() {
		return nil
	}
	value, has := entry[valueKey]
	if !has {
		return nil
	}
	return value
}

// Writes a key into a TTL memo file and drops expired siblings
func ttlMemoPut(path, key string, value any, ttl time.Duration, tsKey, valueKey, what string) {
	now := float64(wallNow().Unix())
	data := map[string]any{}
	for k, v := range loadJSONDict(path) {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		stamp, _ := config.AsFloat(entry[tsKey])
		if now-stamp < ttl.Seconds() {
			data[k] = entry
		}
	}
	data[key] = map[string]any{tsKey: now, valueKey: value}
	if err := atomicJSONWrite(path, data); err != nil {
		metadataLog.Debug("failed to save "+what, "error", err)
	}
}

func localProbeDiskCachePath(ctx context.Context) string {
	return metadataCacheFile(ctx, "local_endpoint_probes.json")
}

func localProbeDiskGet(ctx context.Context, kind, key string) any {
	return ttlMemoGet(localProbeDiskCachePath(ctx), kind+":"+key, localProbeDiskTTL, "ts", "value")
}

func localProbeDiskPut(ctx context.Context, kind, key string, value any) {
	ttlMemoPut(localProbeDiskCachePath(ctx), kind+":"+key, value, localProbeDiskTTL, "ts", "value", "local probe disk cache")
}

func modelMetadataCachePath(ctx context.Context) string {
	return metadataCacheFile(ctx, "openrouter_model_metadata.json")
}

// Disk cache age, negative when freshness is unknown
func modelMetadataDiskCacheAge(ctx context.Context) time.Duration {
	st, err := os.Stat(modelMetadataCachePath(ctx))
	if err != nil {
		return -1
	}
	age := wallNow().Sub(st.ModTime())
	if age < 0 {
		return -1
	}
	return age
}

func loadModelMetadataDiskCache(ctx context.Context) map[string]map[string]any {
	out := map[string]map[string]any{}
	raw, err := os.ReadFile(modelMetadataCachePath(ctx))
	if err != nil {
		metadataLog.Debug("failed to load OpenRouter model metadata disk cache", "error", err)
		return out
	}
	var data any
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &data); err != nil {
		metadataLog.Debug("failed to load OpenRouter model metadata disk cache", "error", err)
		return out
	}
	m, ok := data.(map[string]any)
	if !ok {
		return out
	}
	for k, v := range m {
		if entry, ok := v.(map[string]any); ok {
			out[k] = entry
		}
	}
	return out
}

// Model metadata rows of the on disk cache keyed by model id
func LoadModelMetadataDiskCache(ctx context.Context) map[string]map[string]any {
	return loadModelMetadataDiskCache(ctx)
}

func saveModelMetadataDiskCache(ctx context.Context, data map[string]map[string]any) {
	if err := atomicJSONWrite(modelMetadataCachePath(ctx), data); err != nil {
		metadataLog.Debug("failed to save OpenRouter model metadata disk cache", "error", err)
	}
}

func endpointMetadataCachePath(ctx context.Context) string {
	return metadataCacheFile(ctx, "endpoint_model_metadata.json")
}

// Fresh cross process memo of a remote models probe
func endpointDiskCacheGet(ctx context.Context, normalized string) (map[string]map[string]any, bool) {
	models, ok := ttlMemoGet(endpointMetadataCachePath(ctx), normalized, endpointModelCacheTTL, "at", "models").(map[string]any)
	if !ok {
		return nil, false
	}
	out := map[string]map[string]any{}
	for k, v := range models {
		if entry, ok := v.(map[string]any); ok {
			out[k] = entry
		}
	}
	return out, true
}

func endpointDiskCachePut(ctx context.Context, normalized string, cache map[string]map[string]any) {
	ttlMemoPut(endpointMetadataCachePath(ctx), normalized, cache, endpointModelCacheTTL, "at", "models", "endpoint model metadata disk cache")
}

// Publishes a models catalog in the memo and sweeps expired entries
func rememberEndpointModels(key endpointMemoKey, cache map[string]map[string]any) map[string]map[string]any {
	memoLock.Lock()
	defer memoLock.Unlock()
	now := wallNow()
	DropExpired(endpointModelMetadataCache, endpointModelMetadataCacheTime, endpointModelCacheTTL, now)
	endpointModelMetadataCache[key] = cache
	endpointModelMetadataCacheTime[key] = now
	return cache
}

// Evicts the stalest codex token entry from both context memos before a new key is stored
func boundCodexCaches(cacheKey string) {
	EvictOldest(codexOAuthContextCache, cacheKey, func(entry codexContextCacheEntry) time.Time { return entry.at }, codexCacheMax,
		func(k string) { delete(codexOAuthMaxContextCache, k) })
}

func contextCachePath(ctx context.Context) string {
	return filepath.Join(metadataHome(ctx), "context_length_cache.yaml")
}

// Atomic yaml write of the context cache document
// Context cache writer, a seam the read only cache tests fail on purpose
var writeContextCache = writeContextCacheDocument

func writeContextCacheDocument(ctx context.Context, cache map[string]int, bedrockConfirmed map[string]any) error {
	document := map[string]any{"context_lengths": cache}
	if len(bedrockConfirmed) > 0 {
		document["bedrock_confirmed_v1"] = bedrockConfirmed
	}
	raw, err := yaml.Marshal(document)
	if err != nil {
		return err
	}
	path := contextCachePath(ctx)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return config.AtomicWrite(path, raw, 0o600)
}

var contextCacheProcessLock sync.Mutex

// Runs a load mutate write cycle under the cache file cross process lock, skipped when the lock is busy
func lockedContextCache(ctx context.Context, fn func()) {
	path := contextCachePath(ctx)
	lockPath := path + ".lock"
	contextCacheProcessLock.Lock()
	defer contextCacheProcessLock.Unlock()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		metadataLog.Debug("skipping context length cache update", "error", err)
		return
	}
	f, err := openLockFile(lockPath)
	if err != nil {
		metadataLog.Debug("skipping context length cache update", "error", err)
		return
	}
	defer f.Close()
	deadline := time.Now().Add(contextCacheLockTimeout)
	for {
		err := kernelLock(f, true)
		if err == nil {
			break
		}
		if !isLockContention(err) || time.Now().After(deadline) {
			metadataLog.Debug("skipping context length cache update", "error", err, "lock", lockPath)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer kernelLock(f, false)
	fn()
}

// Loads the context cache document, empty maps when missing or malformed
func loadContextCacheDocument(ctx context.Context) (map[string]int, map[string]any) {
	cache, confirmed := map[string]int{}, map[string]any{}
	raw, err := os.ReadFile(contextCachePath(ctx))
	if err != nil {
		return cache, confirmed
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil || doc == nil {
		return cache, confirmed
	}
	if lengths, ok := doc["context_lengths"].(map[string]any); ok {
		for k, v := range lengths {
			if n, ok := config.AsInt(v); ok {
				cache[k] = n
			}
		}
	}
	if conf, ok := doc["bedrock_confirmed_v1"].(map[string]any); ok {
		confirmed = conf
	}
	return cache, confirmed
}

// Removes every key shape of one cached entry and its Bedrock provenance under the cache lock
func dropContextCacheRows(ctx context.Context, staleKeys []string, label string) {
	lockedContextCache(ctx, func() {
		cache, confirmed := loadContextCacheDocument(ctx)
		hit := false
		for _, k := range staleKeys {
			if _, ok := cache[k]; ok {
				hit = true
			}
			if _, ok := confirmed[k]; ok {
				hit = true
			}
		}
		if !hit {
			return
		}
		for _, k := range staleKeys {
			delete(cache, k)
			delete(confirmed, k)
		}
		if err := writeContextCache(ctx, cache, confirmed); err != nil {
			metadataLog.Debug("failed to invalidate context length cache entry", "entry", label, "error", err)
		}
	})
}

// Sorted keys of a string keyed map for stable iteration
func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func normalizeBaseURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/")
}
