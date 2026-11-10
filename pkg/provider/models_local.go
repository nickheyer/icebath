package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	ollamaDefaultPort          = 11434
	ollamaLocalModelsCacheTTL  = 300 * time.Second
	ollamaLocalProbeFailureTTL = 30 * time.Second
	ollamaLocalCacheMaxEntries = 256
	ollamaCloudCacheTTL        = time.Hour
)

// Drops the first matching path suffix and any trailing slash
func stripSuffixes(root string, suffixes ...string) string {
	for _, suffix := range suffixes {
		if strings.HasSuffix(root, suffix) {
			return strings.TrimRight(root[:len(root)-len(suffix)], "/")
		}
	}
	return root
}

// Adds a usable HTTP scheme without changing an OpenAI API path
func normalizeOpenAIBaseURL(baseURL string) string {
	value := strings.TrimSpace(baseURL)
	if strings.HasPrefix(value, ":") {
		return "http://127.0.0.1" + value
	}
	if value != "" && !strings.Contains(value, "://") {
		return "http://" + value
	}
	return value
}

// Native Ollama API root of an OpenAI style or native base URL
func rootForOllamaNativeAPI(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasPrefix(root, ":") {
		root = "http://127.0.0.1" + root
	} else if root != "" && !strings.Contains(root, "://") {
		root = "http://" + root
	}
	return stripSuffixes(root, "/api/tags", "/v1/models", "/api", "/v1")
}

// The providers.ollama base_url with the legacy api and url keys, empty when unset
func configuredOllamaBaseURL(ctx context.Context) string {
	cfg := getProviderConfigDict(ctx, "ollama")
	for _, key := range []string{"base_url", "api", "url"} {
		if v := pyStr(cfg[key]); pyTruthy(cfg[key]) && v != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Python urlsplit hostname and raw port text of a netloc
func pyNetlocHostPort(netloc string) (hostname, port string, hasPort bool) {
	hostinfo := netloc
	if i := strings.LastIndex(netloc, "@"); i >= 0 {
		hostinfo = netloc[i+1:]
	}
	if _, bracketed, ok := strings.Cut(hostinfo, "["); ok {
		host, rest, _ := strings.Cut(bracketed, "]")
		_, p, _ := strings.Cut(rest, ":")
		return strings.ToLower(host), p, p != ""
	}
	host, p, _ := strings.Cut(hostinfo, ":")
	return strings.ToLower(host), p, p != ""
}

// Python urlsplit port, an error where Python raises ValueError
func pyURLPort(raw string) (int, bool, error) {
	parts, err := pyURLParse(raw, "")
	if err != nil {
		return 0, false, err
	}
	_, port, ok := pyNetlocHostPort(parts.netloc)
	if !ok {
		return 0, false, nil
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return 0, false, fmt.Errorf("Port could not be cast to integer value as %q", port)
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n > 65535 {
		return 0, false, errors.New("Port out of range 0-65535")
	}
	return n, true, nil
}

// Applies the OLLAMA_HOST defaulting rules of port 11434 and IPv6 bracketing
func ollamaHostFromEnv(envHost string) string {
	port := strconv.Itoa(ollamaDefaultPort)
	if strings.HasPrefix(envHost, ":") && !strings.HasPrefix(envHost, "::") {
		return "127.0.0.1" + envHost
	}
	if strings.HasPrefix(envHost, "[") && strings.HasSuffix(envHost, "]") {
		return envHost + ":" + port
	}
	if strings.Contains(envHost, "://") {
		parts, err := pyURLParse(envHost, "")
		if err == nil {
			hostname, _, hasPort := pyNetlocHostPort(parts.netloc)
			if _, _, perr := pyURLPort(envHost); perr == nil && hostname != "" && !hasPort {
				if strings.Contains(hostname, ":") && !strings.HasPrefix(hostname, "[") {
					hostname = "[" + hostname + "]"
				}
				userinfo := ""
				if i := strings.LastIndex(parts.netloc, "@"); i >= 0 {
					userinfo = parts.netloc[:i] + "@"
				}
				parts.netloc = userinfo + hostname + ":" + port
				return pyURLUnparse(parts)
			}
		}
		return envHost
	}
	if strings.Count(envHost, ":") > 1 && !strings.HasPrefix(envHost, "[") {
		return "[" + envHost + "]:" + port
	}
	if !strings.Contains(envHost, ":") {
		return envHost + ":" + port
	}
	return envHost
}

// Local Ollama endpoint from providers.ollama, the active ollama or Ollama serving custom model, OLLAMA_HOST, then the default
func getOllamaBaseURL(ctx context.Context) string {
	if configured := configuredOllamaBaseURL(ctx); configured != "" {
		return configured
	}
	modelCfg := modelConfigDictFn(ctx)
	modelProvider := strings.ToLower(strings.TrimSpace(pyOrStr(modelCfg["provider"])))
	modelBase := strings.TrimSpace(pyOrStr(modelCfg["base_url"]))
	if modelProvider == "ollama" && modelBase != "" {
		return modelBase
	}
	if modelProvider == "custom" && modelBase != "" && shouldUseOllamaNativeCatalog(ctx, "custom", modelBase, nil) {
		return modelBase
	}
	if envHost := strings.TrimSpace(os.Getenv("OLLAMA_HOST")); envHost != "" {
		return ollamaHostFromEnv(envHost)
	}
	return "http://localhost:11434"
}

// The api_key of a provider block, else the secret named by the first set env key field
func apiKeyFromProviderConfig(ctx context.Context, entry map[string]any, envKeys ...string) string {
	if apiKey := strings.TrimSpace(pyOrStr(entry["api_key"])); apiKey != "" {
		return apiKey
	}
	keyEnv := ""
	for _, k := range envKeys {
		if pyTruthy(entry[k]) {
			keyEnv = strings.TrimSpace(pyStr(entry[k]))
			break
		}
	}
	if keyEnv == "" {
		return ""
	}
	return strings.TrimSpace(GetSecretStr(ctx, keyEnv, ""))
}

// Removes every Authorization header variant in place
func dropAuthorization(headers map[string]string) {
	for key := range headers {
		if strings.EqualFold(key, "authorization") {
			delete(headers, key)
		}
	}
}

func hasAuthorization(headers map[string]string) bool {
	for key := range headers {
		if strings.EqualFold(key, "authorization") {
			return true
		}
	}
	return false
}

// Configured extra headers plus the configured bearer for native Ollama requests
func getOllamaRequestHeaders(ctx context.Context) map[string]string {
	entry := getProviderConfigDict(ctx, "ollama")
	result := NormalizeExtraHeaders(entry["extra_headers"])
	if result == nil {
		result = map[string]string{}
	}
	apiKey := apiKeyFromProviderConfig(ctx, entry, "key_env", "api_key_env")
	if apiKey != "" && !hasAuthorization(result) {
		result["Authorization"] = "Bearer " + apiKey
	}
	return result
}

// Ollama headers for one endpoint, configured ones only on the configured root, an explicit key replaces any bearer
func getOllamaNativeHeaders(ctx context.Context, baseURL string, apiKey any) map[string]string {
	configuredBase := configuredOllamaBaseURL(ctx)
	explicitKey := modelsMaterializeProbeAPIKey(ctx, apiKey)
	configuredMatches := configuredBase != "" && baseURL != "" && sameOllamaNativeRoot(baseURL, configuredBase)
	if !configuredMatches && explicitKey == "" {
		return map[string]string{}
	}
	headers := map[string]string{}
	if configuredMatches {
		headers = getOllamaRequestHeaders(ctx)
	}
	_, literal := apiKey.(string)
	if explicitKey != "" || (apiKey != nil && !literal) {
		dropAuthorization(headers)
	}
	if explicitKey != "" {
		headers["Authorization"] = "Bearer " + explicitKey
	}
	return headers
}

// Insertion ordered cache so the oldest key is evicted first
type ollamaOrderedCache[V any] struct {
	keys []string
	vals map[string]V
}

func (c *ollamaOrderedCache[V]) get(key string) (V, bool) {
	v, ok := c.vals[key]
	return v, ok
}

func (c *ollamaOrderedCache[V]) pop(key string) {
	if _, ok := c.vals[key]; !ok {
		return
	}
	delete(c.vals, key)
	for i, k := range c.keys {
		if k == key {
			c.keys = append(c.keys[:i:i], c.keys[i+1:]...)
			break
		}
	}
}

func (c *ollamaOrderedCache[V]) clear() {
	c.keys, c.vals = nil, map[string]V{}
}

type ollamaModelsHit struct {
	models []string
	at     time.Time
}

// Native tags caches keyed by root plus header fingerprint, guarded by ollamaCacheMu
var (
	ollamaCacheMu              sync.Mutex
	ollamaLocalModelsCache     = &ollamaOrderedCache[ollamaModelsHit]{vals: map[string]ollamaModelsHit{}}
	ollamaLocalProbeFailures   = &ollamaOrderedCache[time.Time]{vals: map[string]time.Time{}}
	ollamaLocalProbeReachable  = &ollamaOrderedCache[bool]{vals: map[string]bool{}}
	ollamaMonotonic            = time.Now
	ollamaCloudWallClock       = time.Now
	ollamaThinkingTransport    http.RoundTripper
	ollamaLocalCatalogFetchAPI = func(ctx context.Context, apiKey, baseURL string, headers map[string]string) ([]string, bool) {
		return fetchAPIModelsFn(ctx, apiKey, baseURL, 0, "", headers)
	}
)

// Removes a root from the models and reachability caches and every failure key derived from it
func evictRelatedOllamaCacheEntries(key string) {
	ollamaLocalModelsCache.pop(key)
	ollamaLocalProbeReachable.pop(key)
	for _, failureKey := range append([]string(nil), ollamaLocalProbeFailures.keys...) {
		if failureKey == key || strings.HasPrefix(failureKey, key+"|timeout:") {
			ollamaLocalProbeFailures.pop(failureKey)
		}
	}
}

// Stores a cache value, evicting the oldest root and its relatives when the cache is full
func rememberOllamaCache[V any](cache *ollamaOrderedCache[V], key string, value V) {
	if _, ok := cache.vals[key]; !ok && len(cache.vals) >= ollamaLocalCacheMaxEntries {
		first, _, _ := strings.Cut(cache.keys[0], "|timeout:")
		evictRelatedOllamaCacheEntries(first)
	}
	if _, ok := cache.vals[key]; !ok {
		cache.keys = append(cache.keys, key)
	}
	cache.vals[key] = value
}

// Root plus a blake2b fingerprint of the lowercased sorted headers
func ollamaProbeCacheKey(root string, headers map[string]string) string {
	if len(headers) == 0 {
		return root
	}
	pairs := make([][2]string, 0, len(headers))
	for k, v := range headers {
		pairs = append(pairs, [2]string{strings.ToLower(k), v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	var blob strings.Builder
	blob.WriteByte('[')
	for i, p := range pairs {
		if i > 0 {
			blob.WriteByte(',')
		}
		blob.WriteString("[" + pyJSONQuoteUnicode(p[0]) + "," + pyJSONQuoteUnicode(p[1]) + "]")
	}
	blob.WriteByte(']')
	return root + "|headers:" + hex.EncodeToString(blake2bSum([]byte(blob.String()), 8))
}

// JSON string literal as Python json.dumps writes it with ensure_ascii off
func pyJSONQuoteUnicode(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

var blake2bIV = [8]uint64{
	0x6a09e667f3bcc908, 0xbb67ae8584caa73b, 0x3c6ef372fe94f82b, 0xa54ff53a5f1d36f1,
	0x510e527fade682d1, 0x9b05688c2b3e6c1f, 0x1f83d9abfb41bd6b, 0x5be0cd19137e2179,
}

var blake2bSigma = [12][16]byte{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	{14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
	{11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4},
	{7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8},
	{9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13},
	{2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9},
	{12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11},
	{13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10},
	{6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5},
	{10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0},
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	{14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
}

// Unkeyed BLAKE2b digest of the given size as RFC 7693 defines it
func blake2bSum(data []byte, size int) []byte {
	h := blake2bIV
	h[0] ^= 0x01010000 ^ uint64(size)
	var t uint64
	compress := func(block []byte, last bool) {
		var m [16]uint64
		for i := range m {
			m[i] = binary.LittleEndian.Uint64(block[i*8:])
		}
		var v [16]uint64
		copy(v[:8], h[:])
		copy(v[8:], blake2bIV[:])
		v[12] ^= t
		if last {
			v[14] = ^v[14]
		}
		g := func(a, b, c, d int, x, y uint64) {
			v[a] = v[a] + v[b] + x
			v[d] = bits.RotateLeft64(v[d]^v[a], -32)
			v[c] = v[c] + v[d]
			v[b] = bits.RotateLeft64(v[b]^v[c], -24)
			v[a] = v[a] + v[b] + y
			v[d] = bits.RotateLeft64(v[d]^v[a], -16)
			v[c] = v[c] + v[d]
			v[b] = bits.RotateLeft64(v[b]^v[c], -63)
		}
		for r := 0; r < 12; r++ {
			s := blake2bSigma[r]
			g(0, 4, 8, 12, m[s[0]], m[s[1]])
			g(1, 5, 9, 13, m[s[2]], m[s[3]])
			g(2, 6, 10, 14, m[s[4]], m[s[5]])
			g(3, 7, 11, 15, m[s[6]], m[s[7]])
			g(0, 5, 10, 15, m[s[8]], m[s[9]])
			g(1, 6, 11, 12, m[s[10]], m[s[11]])
			g(2, 7, 8, 13, m[s[12]], m[s[13]])
			g(3, 4, 9, 14, m[s[14]], m[s[15]])
		}
		for i := 0; i < 8; i++ {
			h[i] ^= v[i] ^ v[i+8]
		}
	}
	for len(data) > 128 {
		t += 128
		compress(data[:128], false)
		data = data[128:]
	}
	var last [128]byte
	copy(last[:], data)
	t += uint64(len(data))
	compress(last[:], true)
	out := make([]byte, 64)
	for i, w := range h {
		binary.LittleEndian.PutUint64(out[i*8:], w)
	}
	return out[:size]
}

// BLAKE2b digest of data with the given output size in bytes
func Blake2bSum(data []byte, size int) []byte { return blake2bSum(data, size) }

// Model ids of an api tags payload, false when the shape is not Ollama's
func parseOllamaTags(payload any) ([]string, bool) {
	obj, ok := payload.(map[string]any)
	if !ok {
		return nil, false
	}
	rawModels, ok := obj["models"].([]any)
	if !ok {
		return nil, false
	}
	models := []string{}
	for _, item := range rawModels {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		id := entry["model"]
		if !pyTruthy(id) {
			id = entry["name"]
		}
		modelID := ""
		if pyTruthy(id) {
			modelID = strings.TrimSpace(pyStr(id))
		}
		if modelID != "" && !containsString(models, modelID) {
			models = append(models, modelID)
		}
	}
	if len(rawModels) > 0 && len(models) == 0 {
		return nil, false
	}
	return models, true
}

// ProbeOllamaLocalModels lists the native api tags catalog, false when the endpoint is unreachable or malformed
func ProbeOllamaLocalModels(ctx context.Context, baseURL string, timeout time.Duration, headers map[string]string) ([]string, bool) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if baseURL == "" {
		baseURL = getOllamaBaseURL(ctx)
	}
	root := rootForOllamaNativeAPI(baseURL)
	if root == "" {
		return nil, false
	}
	cacheKey := ollamaProbeCacheKey(root, headers)
	failureKey := fmt.Sprintf("%s|timeout:%.3f", cacheKey, timeout.Seconds())
	ollamaCacheMu.Lock()
	if hit, ok := ollamaLocalModelsCache.get(cacheKey); ok && ollamaMonotonic().Sub(hit.at) < ollamaLocalModelsCacheTTL {
		ollamaCacheMu.Unlock()
		return cloneStrings(hit.models), true
	}
	if failedAt, ok := ollamaLocalProbeFailures.get(failureKey); ok {
		if ollamaMonotonic().Sub(failedAt) < ollamaLocalProbeFailureTTL {
			ollamaCacheMu.Unlock()
			return nil, false
		}
		ollamaLocalProbeFailures.pop(failureKey)
	}
	ollamaCacheMu.Unlock()
	requestHeaders := map[string]string{"User-Agent": userAgent("icebath-cli")}
	for k, v := range headers {
		requestHeaders[k] = v
	}
	var models []string
	ok := false
	if payload, err := modelsGetJSON(ctx, strings.TrimRight(root, "/")+"/api/tags", timeout, requestHeaders, nil); err == nil {
		models, ok = parseOllamaTags(payload)
	}
	ollamaCacheMu.Lock()
	defer ollamaCacheMu.Unlock()
	if !ok {
		rememberOllamaCache(ollamaLocalProbeReachable, cacheKey, false)
		rememberOllamaCache(ollamaLocalProbeFailures, failureKey, ollamaMonotonic())
		return nil, false
	}
	rememberOllamaCache(ollamaLocalProbeReachable, cacheKey, true)
	ollamaLocalProbeFailures.pop(failureKey)
	rememberOllamaCache(ollamaLocalModelsCache, cacheKey, ollamaModelsHit{cloneStrings(models), ollamaMonotonic()})
	return models, true
}

// FetchOllamaLocalModels lists local Ollama models keeping a probe failure as false
func FetchOllamaLocalModels(ctx context.Context, baseURL string, timeout time.Duration, headers map[string]string) ([]string, bool) {
	return ProbeOllamaLocalModels(ctx, baseURL, timeout, headers)
}

// True when two Ollama or OpenAI style base URLs share an API root
func sameOllamaNativeRoot(left, right string) bool {
	leftRoot := strings.TrimRight(rootForOllamaNativeAPI(left), "/")
	rightRoot := strings.TrimRight(rootForOllamaNativeAPI(right), "/")
	if leftRoot == "" || rightRoot == "" {
		return false
	}
	leftParts, lerr := pyURLParse(leftRoot, "")
	rightParts, rerr := pyURLParse(rightRoot, "")
	if lerr != nil || rerr != nil {
		return false
	}
	return urlOrigin(leftRoot) == urlOrigin(rightRoot) &&
		strings.TrimRight(leftParts.path, "/") == strings.TrimRight(rightParts.path, "/")
}

var (
	neverOllamaProviders = setOf("openrouter", "nous", "anthropic", "openai", "openai-codex", "gemini", "ollama-cloud")
	localLikeProviders   = setOf("", "custom", "local", "llamacpp", "llama.cpp", "llama-cpp", "vllm")
)

// ShouldUseOllamaNativeCatalog reports whether discovery should read the native api tags catalog of an endpoint
func ShouldUseOllamaNativeCatalog(ctx context.Context, provider, baseURL string, headers map[string]string) bool {
	return shouldUseOllamaNativeCatalog(ctx, provider, baseURL, headers)
}

func shouldUseOllamaNativeCatalog(ctx context.Context, provider, baseURL string, headers map[string]string) bool {
	requested := strings.ToLower(strings.TrimSpace(provider))
	root := rootForOllamaNativeAPI(baseURL)
	if root != "" {
		if parts, err := pyURLParse(root, ""); err == nil {
			host, _, _ := pyNetlocHostPort(parts.netloc)
			if host == "ollama.com" || strings.HasSuffix(host, ".ollama.com") {
				return false
			}
		}
	}
	if neverOllamaProviders[requested] {
		return false
	}
	configuredBase := configuredOllamaBaseURL(ctx)
	if requested == "ollama" {
		if root == "" {
			return false
		}
		if configuredBase != "" && !sameOllamaNativeRoot(root, configuredBase) {
			_, ok := ProbeOllamaLocalModels(ctx, root, 500*time.Millisecond, headers)
			return ok
		}
		return true
	}
	if configuredBase != "" && sameOllamaNativeRoot(root, configuredBase) {
		return true
	}
	if root == "" {
		return false
	}
	if !localLikeProviders[requested] && !strings.HasPrefix(requested, "custom:") {
		return false
	}
	if requested == "custom:ollama" || strings.HasSuffix(requested, "-ollama") {
		return true
	}
	port, hasPort, err := pyURLPort(root)
	if err != nil || !hasPort || port != ollamaDefaultPort {
		return false
	}
	_, ok := ProbeOllamaLocalModels(ctx, root, 500*time.Millisecond, headers)
	return ok
}

// Raw ollama provider catalog, native tags on a real Ollama server else the configured gateway models listing
func ollamaLocalCatalog(ctx context.Context, forceRefresh bool) []string {
	if forceRefresh {
		clearOllamaLocalCaches()
	}
	baseURL := getOllamaBaseURL(ctx)
	headers := getOllamaNativeHeaders(ctx, baseURL, nil)
	if shouldUseOllamaNativeCatalog(ctx, "ollama", baseURL, headers) {
		native, _ := FetchOllamaLocalModels(ctx, baseURL, 0, headers)
		nativeKey := ollamaProbeCacheKey(rootForOllamaNativeAPI(baseURL), headers)
		ollamaCacheMu.Lock()
		reachable, _ := ollamaLocalProbeReachable.get(nativeKey)
		ollamaCacheMu.Unlock()
		if len(native) > 0 || reachable {
			if native == nil {
				return []string{}
			}
			return native
		}
	}
	cfg := getProviderConfigDict(ctx, "ollama")
	fallbackKey := apiKeyFromProviderConfig(ctx, cfg, "key_env")
	fallbackSource := baseURL
	if pyTruthy(cfg["base_url"]) {
		fallbackSource = pyStr(cfg["base_url"])
	}
	fallbackBase := normalizeOpenAIBaseURL(fallbackSource)
	var fallbackHeaders map[string]string
	if fallbackKey != "" {
		fallbackHeaders = getOllamaNativeHeaders(ctx, fallbackBase, fallbackKey)
	} else {
		fallbackHeaders = getOllamaNativeHeaders(ctx, fallbackBase, nil)
	}
	if len(fallbackHeaders) == 0 {
		fallbackHeaders = nil
	}
	ids, _ := ollamaLocalCatalogFetchAPI(ctx, fallbackKey, fallbackBase, fallbackHeaders)
	if ids == nil {
		return []string{}
	}
	return ids
}

// Whether the configured local Ollama root answered the native tags probe
func ollamaNativeProbeReachable(ctx context.Context) bool {
	baseURL := getOllamaBaseURL(ctx)
	headers := getOllamaNativeHeaders(ctx, baseURL, nil)
	key := ollamaProbeCacheKey(rootForOllamaNativeAPI(baseURL), headers)
	ollamaCacheMu.Lock()
	defer ollamaCacheMu.Unlock()
	reachable, _ := ollamaLocalProbeReachable.get(key)
	return reachable
}

// Empties the native tags model, failure and reachability caches
func clearOllamaLocalCaches() {
	ollamaCacheMu.Lock()
	defer ollamaCacheMu.Unlock()
	ollamaLocalModelsCache.clear()
	ollamaLocalProbeFailures.clear()
	ollamaLocalProbeReachable.clear()
}

// Sends one catalog request with the credential dropping redirect policy, the body on a 2xx answer
func localCatalogDo(ctx context.Context, method, rawURL string, body []byte, timeout time.Duration, headers map[string]string, transport http.RoundTripper, follow bool) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if transport == nil {
		transport = modelsTransport
	}
	if transport == nil {
		transport = &http.Transport{
			Proxy:               probeProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
			TLSHandshakeTimeout: timeout,
		}
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !follow {
				return http.ErrUseLastResponse
			}
			return credentialRedirectPolicy(rawURL)(req, via)
		},
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &modelsHTTPError{URL: rawURL, Status: resp.StatusCode, Body: data}
	}
	return data, nil
}

// JSON decode keeping integers distinct from floats as Python json does
func decodeJSONNumbers(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("extra data after JSON value")
	}
	return out, nil
}

// LM Studio server root without the OpenAI or native API suffix, empty when no URL is given
func lmstudioNativeServerRoot(baseURL string) string {
	return stripSuffixes(strings.TrimRight(strings.TrimSpace(baseURL), "/"), "/api/v1", "/api", "/v1")
}

// User agent plus the bearer for LM Studio native API requests
func lmstudioRequestHeaders(ctx context.Context, apiKey any) map[string]string {
	headers := map[string]string{"User-Agent": userAgent("icebath-cli")}
	if token := modelsMaterializeProbeAPIKey(ctx, apiKey); token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return headers
}

// Raw api v1 models list of LM Studio, false on network or shape failures, an AuthError on 401 or 403
func lmstudioFetchRawModels(ctx context.Context, apiKey any, baseURL string, timeout time.Duration) ([]any, bool, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	serverRoot := lmstudioNativeServerRoot(baseURL)
	if serverRoot == "" {
		return nil, false, nil
	}
	data, err := localCatalogDo(ctx, http.MethodGet, serverRoot+"/api/v1/models", nil, timeout, lmstudioRequestHeaders(ctx, apiKey), nil, true)
	if err != nil {
		var httpErr *modelsHTTPError
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return nil, false, &AuthError{
				Message:  fmt.Sprintf("LM Studio rejected the request with HTTP %d.", httpErr.Status),
				Provider: "lmstudio",
				Code:     "auth_rejected",
				Err:      err,
			}
		}
		return nil, false, nil
	}
	payload, err := decodeJSONNumbers(data)
	if err != nil {
		return nil, false, nil
	}
	obj, _ := payload.(map[string]any)
	rawModels, ok := obj["models"].([]any)
	if !ok {
		return nil, false, nil
	}
	return rawModels, true, nil
}

// Raw LM Studio models with every failure including auth rejection read as none
func lmstudioRawModelsOrNone(ctx context.Context, apiKey any, baseURL string, timeout time.Duration) []any {
	raw, _, _ := lmstudioFetchRawModels(ctx, apiKey, baseURL, timeout)
	return raw
}

// The raw LM Studio entry whose key or id is the model
func lmstudioEntryFor(rawModels []any, model string) map[string]any {
	for _, raw := range rawModels {
		if entry, ok := raw.(map[string]any); ok {
			if key, isStr := entry["key"].(string); isStr && key == model {
				return entry
			}
			if id, isStr := entry["id"].(string); isStr && id == model {
				return entry
			}
		}
	}
	return nil
}

// ProbeLMStudioModels lists chat capable LM Studio keys, false when unreachable, an AuthError on 401 or 403
func ProbeLMStudioModels(ctx context.Context, apiKey any, baseURL string, timeout time.Duration) ([]string, bool, error) {
	rawModels, ok, err := lmstudioFetchRawModels(ctx, apiKey, baseURL, timeout)
	if err != nil || !ok {
		return nil, false, err
	}
	keys := []string{}
	for _, raw := range rawModels {
		entry, isMap := raw.(map[string]any)
		if !isMap || strings.ToLower(strings.TrimSpace(pyOrStr(entry["type"]))) == "embedding" {
			continue
		}
		key := entry["key"]
		if !pyTruthy(key) {
			key = entry["id"]
		}
		id := strings.TrimSpace(pyOrStr(key))
		if id != "" && !containsString(keys, id) {
			keys = append(keys, id)
		}
	}
	return keys, true, nil
}

// FetchLMStudioModels lists chat capable LM Studio keys, empty when unreachable, an AuthError on 401 or 403
func FetchLMStudioModels(ctx context.Context, apiKey any, baseURL string, timeout time.Duration) ([]string, error) {
	keys, _, err := ProbeLMStudioModels(ctx, apiKey, baseURL, timeout)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		return []string{}, nil
	}
	return keys, nil
}

// LMStudioLoadResult is the verified LM Studio runtime context plus load attempt provenance
type LMStudioLoadResult struct {
	ContextLength int
	LoadAttempted bool
	Rejected      bool
}

// Positive JSON integer, booleans and floats excluded
func lmstudioPositiveInt(value any) (int, bool) {
	switch v := value.(type) {
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			return 0, false
		}
		n, err := strconv.Atoi(string(v))
		return n, err == nil && n > 0
	case int:
		return v, v > 0
	case int64:
		return int(v), v > 0
	}
	return 0, false
}

// First positive loaded instance context length of an LM Studio model entry
func lmstudioEntryLoadedContext(entry map[string]any) (int, bool) {
	if entry == nil {
		return 0, false
	}
	instances, ok := entry["loaded_instances"].([]any)
	if !ok {
		return 0, false
	}
	for _, instance := range instances {
		inst, _ := instance.(map[string]any)
		config, _ := inst["config"].(map[string]any)
		if n, ok := lmstudioPositiveInt(config["context_length"]); ok {
			return n, true
		}
	}
	return 0, false
}

// EnsureLMStudioModelLoaded returns the loaded model's verified context, cold loading it with the explicit target when one is set
func EnsureLMStudioModelLoaded(ctx context.Context, model, baseURL string, apiKey any, targetContextLength int, hasTarget bool, timeout time.Duration) LMStudioLoadResult {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	serverRoot := lmstudioNativeServerRoot(baseURL)
	if serverRoot == "" {
		return LMStudioLoadResult{}
	}
	explicit, hasExplicit := 0, false
	if hasTarget {
		if targetContextLength <= 0 {
			return LMStudioLoadResult{}
		}
		explicit, hasExplicit = targetContextLength, true
	}
	target := lmstudioEntryFor(lmstudioRawModelsOrNone(ctx, apiKey, baseURL, 10*time.Second), model)
	if target == nil {
		return LMStudioLoadResult{}
	}
	if maxCtx, ok := lmstudioPositiveInt(target["max_context_length"]); hasExplicit && ok && explicit > maxCtx {
		return LMStudioLoadResult{Rejected: true}
	}
	if current, ok := lmstudioEntryLoadedContext(target); ok {
		return LMStudioLoadResult{ContextLength: current}
	}
	loaded, ok := target["loaded_instances"].([]any)
	if !ok || len(loaded) > 0 {
		return LMStudioLoadResult{}
	}
	payload := newOmap()
	payload.Set("model", model)
	payload.Set("echo_load_config", true)
	if hasExplicit {
		payload.Set("context_length", explicit)
	}
	body, _ := json.Marshal(payload)
	headers := lmstudioRequestHeaders(ctx, apiKey)
	headers["Content-Type"] = "application/json"
	responseBody, err := localCatalogDo(ctx, http.MethodPost, serverRoot+"/api/v1/models/load", body, timeout, headers, nil, true)
	if err != nil {
		return LMStudioLoadResult{LoadAttempted: true}
	}
	response, _ := decodeJSONNumbers(responseBody)
	respObj, _ := response.(map[string]any)
	if loadConfig, ok := respObj["load_config"].(map[string]any); ok {
		if applied, ok := lmstudioPositiveInt(loadConfig["context_length"]); ok {
			return LMStudioLoadResult{ContextLength: applied, LoadAttempted: true}
		}
	}
	refreshed := lmstudioRawModelsOrNone(ctx, apiKey, baseURL, 10*time.Second)
	if refreshed == nil {
		return LMStudioLoadResult{LoadAttempted: true}
	}
	contextLength, _ := lmstudioEntryLoadedContext(lmstudioEntryFor(refreshed, model))
	return LMStudioLoadResult{ContextLength: contextLength, LoadAttempted: true}
}

// LMStudioModelReasoningOptions lists the reasoning allowed options LM Studio publishes for a model
func LMStudioModelReasoningOptions(ctx context.Context, model, baseURL string, apiKey any, timeout time.Duration) []string {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	raw := lmstudioEntryFor(lmstudioRawModelsOrNone(ctx, apiKey, baseURL, timeout), model)
	if raw == nil {
		return []string{}
	}
	caps, _ := raw["capabilities"].(map[string]any)
	reasoning, _ := caps["reasoning"].(map[string]any)
	opts, ok := reasoning["allowed_options"].([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, o := range opts {
		if s, isStr := o.(string); isStr {
			out = append(out, strings.ToLower(strings.TrimSpace(s)))
		}
	}
	return out
}

// OllamaModelSupportsThinking reads thinking from native api show capabilities, known false when the probe failed
func OllamaModelSupportsThinking(ctx context.Context, model, baseURL string, apiKey any, timeout time.Duration) (supported, known bool) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	serverURL := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(baseURL), "/"), "/v1")
	bare := stripOllamaCloudSuffix(strings.TrimSpace(model))
	if serverURL == "" || bare == "" {
		return false, false
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if token := modelsMaterializeProbeAPIKey(ctx, apiKey); token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	body, _ := json.Marshal(map[string]string{"name": bare})
	data, err := localCatalogDo(ctx, http.MethodPost, serverURL+"/api/show", body, timeout, headers, ollamaThinkingTransport, false)
	if err != nil {
		return false, false
	}
	var httpErr *modelsHTTPError
	if errors.As(err, &httpErr) {
		return false, false
	}
	payload, err := decodeJSONNumbers(data)
	if err != nil {
		return false, false
	}
	obj, ok := payload.(map[string]any)
	if !ok {
		return false, false
	}
	caps, ok := obj["capabilities"].([]any)
	if !ok {
		return false, false
	}
	for _, c := range caps {
		if s, isStr := c.(string); isStr && s == "thinking" {
			return true, true
		}
	}
	return false, true
}

// Drops the cloud suffix models.dev appends to Ollama Cloud ids
func stripOllamaCloudSuffix(modelID string) string {
	for _, suffix := range []string{":cloud", "-cloud"} {
		if strings.HasSuffix(modelID, suffix) {
			return modelID[:len(modelID)-len(suffix)]
		}
	}
	return modelID
}

func ollamaCloudCachePath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "ollama_cloud_models_cache.json")
}

// Cached Ollama Cloud model list, false when missing, empty, malformed or older than an hour
func loadOllamaCloudCache(ctx context.Context, ignoreTTL bool) ([]string, bool) {
	data := modelsReadJSONCache(ollamaCloudCachePath(ctx))
	if data == nil {
		return nil, false
	}
	rawModels, ok := data["models"].([]any)
	if !ok || len(rawModels) == 0 {
		return nil, false
	}
	if !ignoreTTL {
		cachedAt := 0.0
		if raw, present := data["cached_at"]; present {
			n, isNum := raw.(float64)
			if !isNum {
				return nil, false
			}
			cachedAt = n
		}
		now := float64(ollamaCloudWallClock().UnixNano()) / 1e9
		if now-cachedAt > ollamaCloudCacheTTL.Seconds() {
			return nil, false
		}
	}
	models := make([]string, 0, len(rawModels))
	for _, m := range rawModels {
		s, isStr := m.(string)
		if !isStr {
			return nil, false
		}
		models = append(models, s)
	}
	return models, true
}

// Persists the merged Ollama Cloud list with its timestamp, a write failure leaves the old file
func saveOllamaCloudCache(ctx context.Context, models []string) {
	now := float64(ollamaCloudWallClock().UnixNano()) / 1e9
	data := pyjson.NewObject()
	data.Set("models", models)
	data.Set("cached_at", now)
	_ = modelsWriteJSONCache(ctx, ollamaCloudCachePath(ctx), data)
}

// OllamaCloudFetch selects the credentials and cache policy of an Ollama Cloud catalog read
type OllamaCloudFetch struct {
	APIKey       string
	BaseURL      string
	ForceRefresh bool
	CacheOnly    bool
}

// FetchOllamaCloudModels returns the fresh disk cache, else live models merged with models.dev, else the stale cache
func FetchOllamaCloudModels(ctx context.Context, opts OllamaCloudFetch) []string {
	if !opts.ForceRefresh {
		if cached, ok := loadOllamaCloudCache(ctx, false); ok {
			return cached
		}
	}
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("OLLAMA_API_KEY")
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = os.Getenv("OLLAMA_BASE_URL")
	}
	if baseURL == "" {
		baseURL = "https://ollama.com/v1"
	}
	var live []string
	if !opts.CacheOnly && apiKey != "" {
		live, _ = fetchAPIModelsFn(ctx, apiKey, baseURL, 8*time.Second, "", nil)
	}
	mdev := listAgenticModelsFn(ctx, "ollama-cloud", true)
	merged := []string{}
	for _, m := range live {
		if m != "" && !containsString(merged, m) {
			merged = append(merged, m)
		}
	}
	for _, m := range mdev {
		if s := stripOllamaCloudSuffix(m); s != "" && !containsString(merged, s) {
			merged = append(merged, s)
		}
	}
	if len(live) > 0 {
		saveOllamaCloudCache(ctx, merged)
		return merged
	}
	if stale, ok := loadOllamaCloudCache(ctx, true); ok {
		return stale
	}
	return merged
}

// Ollama Cloud picker catalog, false when it is empty
func fetchOllamaCloudModels(ctx context.Context, forceRefresh bool) ([]string, bool) {
	ids := FetchOllamaCloudModels(ctx, OllamaCloudFetch{ForceRefresh: forceRefresh})
	return ids, len(ids) > 0
}
