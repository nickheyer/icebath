package local

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	ollamaDefaultPort      = 11434
	ollamaModelsTTL        = 300 * time.Second
	ollamaFailureTTL       = 30 * time.Second
	ollamaCacheMax         = 256
	OllamaProbeTimeout     = 2 * time.Second
	ollamaNativeProbe      = 500 * time.Millisecond
	ollamaCloudTTL         = time.Hour
	ollamaCloudProbe       = 8 * time.Second
	ollamaCloudDefaultBase = "https://ollama.com/v1"
	ollamaLocalDefault     = "http://localhost:11434"
	apiModelsTimeout       = 5 * time.Second
	ollamaShowTimeout      = 5 * time.Second
	ollamaProvider         = "ollama"
	timeoutKeyMarker       = "|timeout:"
)

var (
	neverOllamaProviders = map[string]bool{"openrouter": true, "portal": true, "anthropic": true, "openai": true, "openai-codex": true, "gemini": true, "ollama-cloud": true}
	localLikeProviders   = map[string]bool{"": true, "custom": true, "local": true, "llamacpp": true, "llama.cpp": true, "llama-cpp": true, "vllm": true}
	crossOriginSafe      = map[string]bool{"Accept": true, "User-Agent": true}
)

type ProbeKey struct {
	Static string
	Mint   func(ctx context.Context) (string, error)
}

func (k ProbeKey) token(ctx context.Context) string {
	if k.Mint == nil {
		return strings.TrimSpace(k.Static)
	}
	token, err := k.Mint(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(token)
}

type APIModelLister func(ctx context.Context, apiKey, baseURL string, headers map[string]string, timeout time.Duration) ([]string, bool)

type orderedCache[V any] struct {
	keys   []string
	values map[string]V
}

func newOrderedCache[V any]() orderedCache[V] { return orderedCache[V]{values: map[string]V{}} }

func (c *orderedCache[V]) get(key string) (V, bool) {
	v, ok := c.values[key]
	return v, ok
}

func (c *orderedCache[V]) del(key string) {
	if _, ok := c.values[key]; !ok {
		return
	}
	delete(c.values, key)
	for i, k := range c.keys {
		if k == key {
			c.keys = append(c.keys[:i], c.keys[i+1:]...)
			break
		}
	}
}

func (c *orderedCache[V]) reset() {
	c.keys, c.values = nil, map[string]V{}
}

type ollamaHit struct {
	models []string
	at     time.Time
}

type ollamaCaches struct {
	mu        sync.Mutex
	models    orderedCache[ollamaHit]
	failures  orderedCache[time.Time]
	reachable orderedCache[bool]
}

func newOllamaCaches() ollamaCaches {
	return ollamaCaches{models: newOrderedCache[ollamaHit](), failures: newOrderedCache[time.Time](), reachable: newOrderedCache[bool]()}
}

func (o *ollamaCaches) evictRelated(key string) {
	o.models.del(key)
	o.reachable.del(key)
	for _, k := range append([]string(nil), o.failures.keys...) {
		if k == key || strings.HasPrefix(k, key+timeoutKeyMarker) {
			o.failures.del(k)
		}
	}
}

func remember[V any](o *ollamaCaches, c *orderedCache[V], key string, value V) {
	if _, ok := c.values[key]; !ok && len(c.values) >= ollamaCacheMax {
		first, _, _ := strings.Cut(c.keys[0], timeoutKeyMarker)
		o.evictRelated(first)
	}
	if _, ok := c.values[key]; !ok {
		c.keys = append(c.keys, key)
	}
	c.values[key] = value
}

func stripSuffixes(root string, suffixes ...string) string {
	for _, s := range suffixes {
		if strings.HasSuffix(root, s) {
			return strings.TrimRight(root[:len(root)-len(s)], "/")
		}
	}
	return root
}

func NormalizeOpenAIBaseURL(baseURL string) string {
	value := strings.TrimSpace(baseURL)
	if strings.HasPrefix(value, ":") {
		return "http://127.0.0.1" + value
	}
	if value != "" && !strings.Contains(value, "://") {
		return "http://" + value
	}
	return value
}

func OllamaNativeRoot(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasPrefix(root, ":") {
		root = "http://127.0.0.1" + root
	} else if root != "" && !strings.Contains(root, "://") {
		root = "http://" + root
	}
	return stripSuffixes(root, "/api/tags", "/v1/models", "/api", "/v1")
}

func ollamaProviderEntry(cfg *config.Config) config.ProviderEntry {
	if entry, ok := cfg.Providers.Get(ollamaProvider); ok {
		return entry
	}
	return config.ProviderEntry{}
}

func configuredOllamaBaseURL(cfg *config.Config) string {
	entry := ollamaProviderEntry(cfg)
	for _, v := range []string{entry.BaseURL, entry.API, entry.URL} {
		if v != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func ollamaHostFromEnv(envHost string) string {
	port := strconv.Itoa(ollamaDefaultPort)
	switch {
	case strings.HasPrefix(envHost, ":") && !strings.HasPrefix(envHost, "::"):
		return "127.0.0.1" + envHost
	case strings.HasPrefix(envHost, "[") && strings.HasSuffix(envHost, "]"):
		return envHost + ":" + port
	case strings.Contains(envHost, "://"):
		u, err := url.Parse(envHost)
		if err == nil && u.Hostname() != "" && u.Port() == "" {
			hostname := u.Hostname()
			if strings.Contains(hostname, ":") && !strings.HasPrefix(hostname, "[") {
				hostname = "[" + hostname + "]"
			}
			u.Host = hostname + ":" + port
			return u.String()
		}
		return envHost
	case strings.Count(envHost, ":") > 1 && !strings.HasPrefix(envHost, "["):
		return "[" + envHost + "]:" + port
	case !strings.Contains(envHost, ":"):
		return envHost + ":" + port
	}
	return envHost
}

func (r *Runtime) OllamaBaseURL(ctx context.Context) (string, error) {
	cfg, err := r.config(ctx)
	if err != nil {
		return "", err
	}
	if configured := configuredOllamaBaseURL(cfg); configured != "" {
		return configured, nil
	}
	if !cfg.Model.Bare {
		provider := strings.ToLower(strings.TrimSpace(cfg.Model.Provider))
		base := strings.TrimSpace(cfg.Model.BaseURL)
		if provider == ollamaProvider && base != "" {
			return base, nil
		}
		if provider == "custom" && base != "" {
			if native, err := r.ShouldUseOllamaNativeCatalog(ctx, "custom", base, nil); err == nil && native {
				return base, nil
			}
		}
	}
	if envHost := strings.TrimSpace(os.Getenv("OLLAMA_HOST")); envHost != "" {
		return ollamaHostFromEnv(envHost), nil
	}
	return ollamaLocalDefault, nil
}

func (r *Runtime) apiKeyFromProviderEntry(ctx context.Context, entry config.ProviderEntry, envKeys ...string) (string, error) {
	if key := strings.TrimSpace(entry.APIKey); key != "" {
		return key, nil
	}
	var keyEnv string
	for _, name := range envKeys {
		switch name {
		case "key_env":
			keyEnv = entry.KeyEnv
		case "api_key_env":
			keyEnv = entry.APIKeyEnv
		}
		if keyEnv != "" {
			break
		}
	}
	keyEnv = strings.TrimSpace(keyEnv)
	if keyEnv == "" {
		return "", nil
	}
	value, err := r.secrets.Get(ctx, keyEnv, "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func dropAuthorization(headers map[string]string) {
	for key := range headers {
		if strings.EqualFold(key, "authorization") {
			delete(headers, key)
		}
	}
}

func (r *Runtime) OllamaRequestHeaders(ctx context.Context) (map[string]string, error) {
	cfg, err := r.config(ctx)
	if err != nil {
		return nil, err
	}
	entry := ollamaProviderEntry(cfg)
	out := map[string]string{}
	for _, key := range entry.ExtraHeaders.Keys() {
		value, _ := entry.ExtraHeaders.Get(key)
		out[key] = value
	}
	apiKey, err := r.apiKeyFromProviderEntry(ctx, entry, "key_env", "api_key_env")
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		has := false
		for key := range out {
			if strings.EqualFold(key, "authorization") {
				has = true
			}
		}
		if !has {
			out["Authorization"] = "Bearer " + apiKey
		}
	}
	return out, nil
}

func (r *Runtime) OllamaNativeHeaders(ctx context.Context, baseURL string, key ProbeKey) (map[string]string, error) {
	cfg, err := r.config(ctx)
	if err != nil {
		return nil, err
	}
	configured := configuredOllamaBaseURL(cfg)
	explicit := key.token(ctx)
	matches := configured != "" && baseURL != "" && SameOllamaNativeRoot(baseURL, configured)
	if !matches && explicit == "" {
		return map[string]string{}, nil
	}
	headers := map[string]string{}
	if matches {
		if headers, err = r.OllamaRequestHeaders(ctx); err != nil {
			return nil, err
		}
	}
	if explicit != "" || key.Mint != nil {
		dropAuthorization(headers)
	}
	if explicit != "" {
		headers["Authorization"] = "Bearer " + explicit
	}
	return headers, nil
}

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
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(pairs)
	h, _ := blake2b.New(8, nil)
	h.Write(bytes.TrimRight(buf.Bytes(), "\n"))
	return root + "|headers:" + hex.EncodeToString(h.Sum(nil))
}

func parseOllamaTags(payload any) ([]string, bool) {
	doc, ok := payload.(map[string]any)
	if !ok {
		return nil, false
	}
	raw, ok := doc["models"].([]any)
	if !ok {
		return nil, false
	}
	models := []string{}
	seen := map[string]bool{}
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		id := m["model"]
		if !jsonTruthy(id) {
			id = m["name"]
		}
		modelID := ""
		if jsonTruthy(id) {
			modelID = strings.TrimSpace(jsonText(id))
		}
		if modelID != "" && !seen[modelID] {
			seen[modelID] = true
			models = append(models, modelID)
		}
	}
	if len(raw) > 0 && len(models) == 0 {
		return nil, false
	}
	return models, true
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		switch strings.ToLower(u.Scheme) {
		case "http":
			return "80"
		case "https":
			return "443"
		}
		return ""
	}
	host := func(u *url.URL) string { return strings.TrimRight(strings.ToLower(u.Hostname()), ".") }
	return strings.EqualFold(a.Scheme, b.Scheme) && host(a) == host(b) && port(a) == port(b)
}

func (r *Runtime) credentialedClient() *http.Client {
	base := r.client
	client := *base
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if base.CheckRedirect != nil {
			if err := base.CheckRedirect(req, via); err != nil {
				return err
			}
		}
		if len(via) > 0 && !sameOrigin(via[0].URL, req.URL) {
			for name := range req.Header {
				if !crossOriginSafe[http.CanonicalHeaderKey(name)] {
					req.Header.Del(name)
				}
			}
		}
		return nil
	}
	return &client
}

func (r *Runtime) catalogGet(ctx context.Context, method, rawURL string, headers map[string]string, body []byte, timeout time.Duration) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.credentialedClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := httpclient.CheckStatus(resp); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

func (r *Runtime) ProbeOllamaLocalModels(ctx context.Context, baseURL string, timeout time.Duration, headers map[string]string) ([]string, bool, error) {
	if baseURL == "" {
		var err error
		if baseURL, err = r.OllamaBaseURL(ctx); err != nil {
			return nil, false, err
		}
	}
	root := OllamaNativeRoot(baseURL)
	if root == "" {
		return nil, false, nil
	}
	if timeout <= 0 {
		timeout = OllamaProbeTimeout
	}
	cacheKey := ollamaProbeCacheKey(root, headers)
	failureKey := fmt.Sprintf("%s%s%.3f", cacheKey, timeoutKeyMarker, timeout.Seconds())
	o := &r.ollama
	o.mu.Lock()
	if hit, ok := o.models.get(cacheKey); ok && time.Since(hit.at) < ollamaModelsTTL {
		o.mu.Unlock()
		return append([]string(nil), hit.models...), true, nil
	}
	if failedAt, ok := o.failures.get(failureKey); ok {
		if time.Since(failedAt) < ollamaFailureTTL {
			o.mu.Unlock()
			return nil, false, nil
		}
		o.failures.del(failureKey)
	}
	o.mu.Unlock()
	var models []string
	parsed := false
	if raw, err := r.catalogGet(ctx, http.MethodGet, strings.TrimRight(root, "/")+"/api/tags", headers, nil, timeout); err == nil {
		var payload any
		if json.Unmarshal(raw, &payload) == nil {
			models, parsed = parseOllamaTags(payload)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !parsed {
		remember(o, &o.reachable, cacheKey, false)
		remember(o, &o.failures, failureKey, time.Now())
		return nil, false, nil
	}
	remember(o, &o.reachable, cacheKey, true)
	o.failures.del(failureKey)
	remember(o, &o.models, cacheKey, ollamaHit{models: append([]string(nil), models...), at: time.Now()})
	return models, true, nil
}

func SameOllamaNativeRoot(left, right string) bool {
	l := strings.TrimRight(OllamaNativeRoot(left), "/")
	r := strings.TrimRight(OllamaNativeRoot(right), "/")
	if l == "" || r == "" {
		return false
	}
	lu, err := url.Parse(l)
	if err != nil {
		return false
	}
	ru, err := url.Parse(r)
	if err != nil {
		return false
	}
	return sameOrigin(lu, ru) && strings.TrimRight(lu.Path, "/") == strings.TrimRight(ru.Path, "/")
}

func (r *Runtime) ShouldUseOllamaNativeCatalog(ctx context.Context, provider, baseURL string, headers map[string]string) (bool, error) {
	requested := strings.ToLower(strings.TrimSpace(provider))
	root := OllamaNativeRoot(baseURL)
	var rootURL *url.URL
	if root != "" {
		if u, err := url.Parse(root); err == nil {
			rootURL = u
			host := strings.ToLower(u.Hostname())
			if host == "ollama.com" || strings.HasSuffix(host, ".ollama.com") {
				return false, nil
			}
		}
	}
	if neverOllamaProviders[requested] {
		return false, nil
	}
	cfg, err := r.config(ctx)
	if err != nil {
		return false, err
	}
	configured := configuredOllamaBaseURL(cfg)
	if requested == ollamaProvider {
		if root == "" {
			return false, nil
		}
		if configured != "" && !SameOllamaNativeRoot(root, configured) {
			_, ok, err := r.ProbeOllamaLocalModels(ctx, root, ollamaNativeProbe, headers)
			return ok, err
		}
		return true, nil
	}
	if configured != "" && SameOllamaNativeRoot(root, configured) {
		return true, nil
	}
	if root == "" {
		return false, nil
	}
	if !localLikeProviders[requested] && !strings.HasPrefix(requested, "custom:") {
		return false, nil
	}
	if requested == "custom:ollama" || strings.HasSuffix(requested, "-ollama") {
		return true, nil
	}
	if rootURL == nil || rootURL.Port() != strconv.Itoa(ollamaDefaultPort) {
		return false, nil
	}
	_, ok, err := r.ProbeOllamaLocalModels(ctx, root, ollamaNativeProbe, headers)
	return ok, err
}

func (r *Runtime) ResetOllamaProbeCaches() {
	r.ollama.mu.Lock()
	r.ollama.models.reset()
	r.ollama.failures.reset()
	r.ollama.reachable.reset()
	r.ollama.mu.Unlock()
}

func (r *Runtime) OllamaLocalCatalog(ctx context.Context, forceRefresh bool, list APIModelLister) ([]string, error) {
	if forceRefresh {
		r.ResetOllamaProbeCaches()
	}
	baseURL, err := r.OllamaBaseURL(ctx)
	if err != nil {
		return nil, err
	}
	headers, err := r.OllamaNativeHeaders(ctx, baseURL, ProbeKey{})
	if err != nil {
		return nil, err
	}
	native, err := r.ShouldUseOllamaNativeCatalog(ctx, ollamaProvider, baseURL, headers)
	if err != nil {
		return nil, err
	}
	if native {
		models, _, err := r.ProbeOllamaLocalModels(ctx, baseURL, OllamaProbeTimeout, headers)
		if err != nil {
			return nil, err
		}
		key := ollamaProbeCacheKey(OllamaNativeRoot(baseURL), headers)
		r.ollama.mu.Lock()
		reachable, _ := r.ollama.reachable.get(key)
		r.ollama.mu.Unlock()
		if len(models) > 0 || reachable {
			if models == nil {
				models = []string{}
			}
			return models, nil
		}
	}
	cfg, err := r.config(ctx)
	if err != nil {
		return nil, err
	}
	entry := ollamaProviderEntry(cfg)
	fallbackKey, err := r.apiKeyFromProviderEntry(ctx, entry, "key_env")
	if err != nil {
		return nil, err
	}
	fallbackBase := entry.BaseURL
	if fallbackBase == "" {
		fallbackBase = baseURL
	}
	fallbackBase = NormalizeOpenAIBaseURL(fallbackBase)
	fallbackHeaders, err := r.OllamaNativeHeaders(ctx, fallbackBase, ProbeKey{Static: fallbackKey})
	if err != nil {
		return nil, err
	}
	if len(fallbackHeaders) == 0 {
		fallbackHeaders = nil
	}
	models, ok := list(ctx, fallbackKey, fallbackBase, fallbackHeaders, apiModelsTimeout)
	if !ok || models == nil {
		return []string{}, nil
	}
	return models, nil
}

func (r *Runtime) OllamaModelSupportsThinking(ctx context.Context, model, baseURL string, key ProbeKey, timeout time.Duration) (bool, bool) {
	serverURL := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(baseURL), "/"), "/v1")
	bare := stripOllamaCloudSuffix(strings.TrimSpace(model))
	if serverURL == "" || bare == "" {
		return false, false
	}
	if timeout <= 0 {
		timeout = ollamaShowTimeout
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if token := key.token(ctx); token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	body, err := json.Marshal(map[string]string{"name": bare})
	if err != nil {
		return false, false
	}
	raw, err := r.plainRequest(ctx, http.MethodPost, serverURL+"/api/show", headers, body, timeout)
	if err != nil {
		return false, false
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return false, false
	}
	caps, ok := doc["capabilities"].([]any)
	if !ok {
		return false, false
	}
	for _, c := range caps {
		if c == "thinking" {
			return true, true
		}
	}
	return false, true
}

func (r *Runtime) plainRequest(ctx context.Context, method, rawURL string, headers map[string]string, body []byte, timeout time.Duration) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func stripOllamaCloudSuffix(modelID string) string {
	for _, suffix := range []string{":cloud", "-cloud"} {
		if strings.HasSuffix(modelID, suffix) {
			return modelID[:len(modelID)-len(suffix)]
		}
	}
	return modelID
}

type ollamaCloudCache struct {
	Models   []string `json:"models"`
	CachedAt float64  `json:"cached_at"`
}

func (r *Runtime) ollamaCloudCachePath(ctx context.Context) (string, error) {
	h, err := r.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	return home.OllamaCloudModelsCache.In(h), nil
}

func (r *Runtime) loadOllamaCloudCache(ctx context.Context, ignoreTTL bool) []string {
	path, err := r.ollamaCloudCachePath(ctx)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &doc) != nil {
		return nil
	}
	var models []any
	if json.Unmarshal(doc["models"], &models) != nil || len(models) == 0 {
		return nil
	}
	if !ignoreTTL {
		cachedAt := 0.0
		if raw, ok := doc["cached_at"]; ok && json.Unmarshal(raw, &cachedAt) != nil {
			return nil
		}
		if float64(time.Now().UnixNano())/1e9-cachedAt > ollamaCloudTTL.Seconds() {
			return nil
		}
	}
	out := make([]string, len(models))
	for i, m := range models {
		out[i] = jsonText(m)
	}
	return out
}

func (r *Runtime) saveOllamaCloudCache(ctx context.Context, models []string) {
	path, err := r.ollamaCloudCachePath(ctx)
	if err != nil {
		return
	}
	if r.homes.Mkdir(ctx, filepath.Dir(path)) != nil {
		return
	}
	doc := ollamaCloudCache{Models: models, CachedAt: float64(time.Now().UnixNano()) / 1e9}
	_ = fileutil.WriteJSON(ctx, path, doc, fileutil.JSONStyle{})
}

type OllamaCloudRequest struct {
	APIKey       string
	BaseURL      string
	ForceRefresh bool
	CacheOnly    bool
	List         APIModelLister
	ModelsDev    func(ctx context.Context) ([]string, error)
}

func (r *Runtime) FetchOllamaCloudModels(ctx context.Context, q OllamaCloudRequest) []string {
	if !q.ForceRefresh {
		if cached := r.loadOllamaCloudCache(ctx, false); cached != nil {
			return cached
		}
	}
	apiKey := q.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("OLLAMA_API_KEY")
	}
	baseURL := q.BaseURL
	if baseURL == "" {
		baseURL = os.Getenv("OLLAMA_BASE_URL")
	}
	if baseURL == "" {
		baseURL = ollamaCloudDefaultBase
	}
	var live []string
	if !q.CacheOnly && apiKey != "" {
		if models, ok := q.List(ctx, apiKey, baseURL, nil, ollamaCloudProbe); ok {
			live = models
		}
	}
	var mdev []string
	if q.ModelsDev != nil {
		if models, err := q.ModelsDev(ctx); err == nil {
			mdev = models
		}
	}
	merged := []string{}
	seen := map[string]bool{}
	add := func(m string) {
		if m != "" && !seen[m] {
			seen[m] = true
			merged = append(merged, m)
		}
	}
	for _, m := range live {
		add(m)
	}
	for _, m := range mdev {
		add(stripOllamaCloudSuffix(m))
	}
	if len(live) > 0 {
		r.saveOllamaCloudCache(ctx, merged)
		return merged
	}
	if stale := r.loadOllamaCloudCache(ctx, true); stale != nil {
		return stale
	}
	return merged
}
