package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Wall clock cap on one probe including the whole body
var probeTotalDeadline = 15 * time.Second

// Transport every probe uses, nil selects the default proxy aware transport
var metadataTransport http.RoundTripper

// OpenRouter catalog endpoint
const OpenRouterModelsURL = home.OpenRouterModelsURL

// Response of one metadata probe with the body already read for successes
type probeResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (r *probeResponse) IsError() bool { return r.StatusCode >= 400 }

// Decoded body as any json value, nil when it does not parse
func (r *probeResponse) JSONAny() any {
	var v any
	if err := json.Unmarshal(bytes.TrimPrefix(r.Body, []byte("\xef\xbb\xbf")), &v); err != nil {
		return nil
	}
	return v
}

// Decoded body when it is a json object, else an empty map
func (r *probeResponse) JSON() map[string]any {
	m, ok := r.JSONAny().(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

func (r *probeResponse) Text() string { return string(r.Body) }

// Reports a dial phase timeout, read timeouts prove the server accepted a connection
func isConnectTimeout(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) {
		return op.Op == "dial" && op.Timeout()
	}
	return false
}

// TLS policy of a probe to the base url from the custom provider entry serving it
func metadataTLSConfig(ctx context.Context, baseURL string) (*tls.Config, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, nil
	}
	cfg := metadataConfig(ctx)
	if cfg == nil {
		return nil, nil
	}
	settings := CustomProviderTLSSettings(baseURL, nil, cfg)
	var sslVerify any
	if settings.HasVerify {
		sslVerify = settings.Verify
	}
	return ResolveTLSVerify(nil, settings.CACert, sslVerify, baseURL).Config, nil
}

// Proxy for a probe request read from the environment on every call
func probeProxyFromEnvironment(req *http.Request) (*url.URL, error) {
	host := strings.ToLower(req.URL.Hostname())
	for _, entry := range strings.Split(firstEnv("NO_PROXY", "no_proxy"), ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" || host == strings.TrimPrefix(entry, ".") || strings.HasSuffix(host, "."+strings.TrimPrefix(entry, ".")) {
			return nil, nil
		}
	}
	var raw string
	switch strings.ToLower(req.URL.Scheme) {
	case "https":
		raw = firstEnv("HTTPS_PROXY", "https_proxy")
	case "http":
		raw = firstEnv("HTTP_PROXY", "http_proxy")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return url.Parse(raw)
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// Metadata probe client sharing chat TLS policy with credentials scoped to the first origin
type probeClient struct {
	ctx     context.Context
	headers map[string]string
	connect time.Duration
	read    time.Duration
	tls     *tls.Config
	tlsErr  error
}

func newProbeClient(ctx context.Context, baseURL string, headers map[string]string, connect, read time.Duration) *probeClient {
	if headers == nil {
		headers = map[string]string{}
	}
	tlsCfg, err := metadataTLSConfig(ctx, baseURL)
	return &probeClient{ctx: ctx, headers: headers, connect: connect, read: read, tls: tlsCfg, tlsErr: err}
}

func (c *probeClient) httpClient(target string) *http.Client {
	var transport http.RoundTripper = &http.Transport{
		Proxy:                 probeProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: c.connect}).DialContext,
		TLSClientConfig:       c.tls,
		ResponseHeaderTimeout: c.read,
		TLSHandshakeTimeout:   c.connect,
	}
	if metadataTransport != nil {
		transport = metadataTransport
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: credentialRedirectPolicy(target),
	}
}

// Sends one request under the total deadline, reading the body only for successes
func (c *probeClient) do(method, rawURL string, params map[string]string, body []byte, extra map[string]string) (*probeResponse, error) {
	if c.tlsErr != nil {
		return nil, c.tlsErr
	}
	target := rawURL
	if len(params) > 0 {
		q := url.Values{}
		for _, k := range sortedStringKeys(params) {
			q.Set(k, params[k])
		}
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + q.Encode()
	}
	origin := urlOrigin(target)
	ctx, cancel := context.WithTimeout(c.ctx, probeTotalDeadline)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient(target).Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && !isConnectTimeout(err) && c.ctx.Err() == nil {
			return nil, &DeadlineExpiredError{Label: "model metadata " + origin, After: probeTotalDeadline}
		}
		return nil, err
	}
	defer resp.Body.Close()
	out := &probeResponse{StatusCode: resp.StatusCode, Header: resp.Header}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		data, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			if errors.Is(rerr, context.DeadlineExceeded) && c.ctx.Err() == nil {
				return nil, &DeadlineExpiredError{Label: "model metadata " + origin, After: probeTotalDeadline}
			}
			return nil, rerr
		}
		out.Body = data
	}
	return out, nil
}

func (c *probeClient) Get(rawURL string, params map[string]string) (*probeResponse, error) {
	return c.do(http.MethodGet, rawURL, params, nil, nil)
}

func (c *probeClient) PostJSON(rawURL string, payload any) (*probeResponse, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return c.do(http.MethodPost, rawURL, nil, raw, map[string]string{"Content-Type": "application/json"})
}

// Error of a non success status from a probe
type probeStatusError struct {
	StatusCode int
	URL        string
}

func (e *probeStatusError) Error() string {
	return fmt.Sprintf("HTTP %d from %s", e.StatusCode, e.URL)
}

func raiseForStatus(resp *probeResponse, rawURL string) error {
	if resp.IsError() {
		return &probeStatusError{StatusCode: resp.StatusCode, URL: rawURL}
	}
	return nil
}

func intOrDefault(v any, fallback int) int {
	if n, ok := intValue(v); ok {
		return n
	}
	return fallback
}

// Positive integer from a numeric value, zero otherwise
func positiveIntOf(v any) int {
	n, ok := positiveInt(v)
	if !ok {
		return 0
	}
	return n
}

// FetchModelMetadata returns the OpenRouter catalog, cached for an hour in memory and on disk
func FetchModelMetadata(ctx context.Context, forceRefresh bool) map[string]map[string]any {
	memoLock.Lock()
	defer memoLock.Unlock()
	if !forceRefresh {
		if len(modelMetadataCache) > 0 && wallNow().Sub(modelMetadataCacheTime) < modelCacheTTL {
			return modelMetadataCache
		}
		if age := modelMetadataDiskCacheAge(ctx); age >= 0 && age < modelCacheTTL {
			if disk := loadModelMetadataDiskCache(ctx); len(disk) > 0 {
				modelMetadataCache = disk
				modelMetadataCacheTime = wallNow().Add(-age)
				return modelMetadataCache
			}
		}
	}
	client := newProbeClient(ctx, "", nil, 5*time.Second, 10*time.Second)
	resp, err := client.Get(OpenRouterModelsURL, nil)
	if err == nil {
		err = raiseForStatus(resp, OpenRouterModelsURL)
	}
	if err == nil {
		cache := map[string]map[string]any{}
		data, _ := resp.JSON()["data"].([]any)
		for _, item := range data {
			model, ok := item.(map[string]any)
			if !ok {
				continue
			}
			modelID := stringOf(model["id"])
			top, _ := model["top_provider"].(map[string]any)
			pricing, _ := model["pricing"].(map[string]any)
			if pricing == nil {
				pricing = map[string]any{}
			}
			name := modelID
			if n, ok := model["name"].(string); ok {
				name = n
			}
			entry := map[string]any{
				"context_length":        intOrDefault(model["context_length"], 128000),
				"max_completion_tokens": intOrDefault(top["max_completion_tokens"], 4096),
				"name":                  name,
				"pricing":               pricing,
			}
			aliases := []string{modelID}
			if canonical := stringOf(model["canonical_slug"]); canonical != "" && canonical != modelID {
				aliases = append(aliases, canonical)
			}
			for _, alias := range aliases {
				addModelAliases(cache, alias, entry)
			}
		}
		modelMetadataCache = cache
		modelMetadataCacheTime = wallNow()
		saveModelMetadataDiskCache(ctx, cache)
		metadataLog.Debug("fetched OpenRouter model metadata", "models", len(cache))
		return cache
	}
	metadataLog.Warn("Failed to fetch model metadata from OpenRouter", "error", err)
	if len(modelMetadataCache) > 0 {
		return modelMetadataCache
	}
	if disk := loadModelMetadataDiskCache(ctx); len(disk) > 0 {
		modelMetadataCache = disk
		staleBy := modelCacheTTL - time.Second
		if age := modelMetadataDiskCacheAge(ctx); age >= 0 {
			staleBy = age
			if staleBy > modelCacheTTL {
				staleBy = modelCacheTTL
			}
		}
		modelMetadataCacheTime = wallNow().Add(-staleBy)
		return modelMetadataCache
	}
	return map[string]map[string]any{}
}

// Cache entry for one models item, optional keys only when known
func endpointModelEntry(model map[string]any, modelID string, contextLength int, hasContext bool) map[string]any {
	name := modelID
	if n, ok := model["name"].(string); ok {
		name = n
	}
	entry := map[string]any{"name": name}
	if hasContext {
		entry["context_length"] = contextLength
	}
	if n, ok := extractFirstInt(model, maxCompletionKeys); ok {
		entry["max_completion_tokens"] = n
	}
	if pricing := extractPricing(model); len(pricing) > 0 {
		entry["pricing"] = pricing
	}
	return entry
}

// Context of the first loaded LM Studio instance
func lmstudioLoadedContext(model map[string]any) (int, bool) {
	instances, _ := model["loaded_instances"].([]any)
	for _, raw := range instances {
		inst, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		cfg, _ := inst["config"].(map[string]any)
		if n, ok := intValue(cfg["context_length"]); ok && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// LM Studio native models list into a cache keyed by key and id
func lmstudioNativeModels(ctx context.Context, normalized string, headers map[string]string) (map[string]map[string]any, error) {
	target := strings.TrimRight(lmstudioServerRoot(normalized), "/") + "/api/v1/models"
	resp, err := newProbeClient(ctx, normalized, headers, 5*time.Second, 10*time.Second).Get(target, nil)
	if err != nil {
		return nil, err
	}
	if err := raiseForStatus(resp, target); err != nil {
		return nil, err
	}
	cache := map[string]map[string]any{}
	models, _ := resp.JSON()["models"].([]any)
	for _, raw := range models {
		model, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		modelID := stringOf(model["key"])
		if modelID == "" {
			modelID = stringOf(model["id"])
		}
		if modelID == "" {
			continue
		}
		loaded, hasLoaded := lmstudioLoadedContext(model)
		entry := endpointModelEntry(model, modelID, loaded, hasLoaded)
		addModelAliases(cache, modelID, entry)
		if alt, ok := model["id"].(string); ok && alt != "" && alt != modelID {
			addModelAliases(cache, alt, entry)
		}
	}
	return cache, nil
}

// Overwrites context_length with the n_ctx llama.cpp allocated for loaded models
func applyLlamacppProps(client *probeClient, cache map[string]map[string]any, requestCandidate string) {
	base := strings.ReplaceAll(strings.TrimRight(requestCandidate, "/"), "/v1", "")
	props := func(params map[string]string) (*probeResponse, error) {
		resp, err := client.Get(base+"/v1/props", params)
		if err != nil {
			return nil, err
		}
		if resp.IsError() {
			return client.Get(base+"/props", params)
		}
		return resp, nil
	}
	nCtx := func(p map[string]any) int {
		settings, _ := p["default_generation_settings"].(map[string]any)
		return positiveIntOf(settings["n_ctx"])
	}
	propsResp, err := props(nil)
	if err != nil {
		return
	}
	if !propsResp.IsError() {
		p := propsResp.JSON()
		alias := stringOf(p["model_alias"])
		if n := nCtx(p); n > 0 && alias != "" {
			if entry, ok := cache[alias]; ok {
				entry["context_length"] = n
			}
		}
		return
	}
	native, err := client.Get(base+"/models", nil)
	if err != nil || native.IsError() {
		return
	}
	data, _ := native.JSON()["data"].([]any)
	if len(data) > 16 {
		data = data[:16]
	}
	for _, raw := range data {
		child, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		childID := stringOf(child["id"])
		entry, known := cache[childID]
		status, _ := child["status"].(map[string]any)
		value := stringOf(status["value"])
		if childID == "" || !known || (value != "loaded" && value != "ready") {
			continue
		}
		pr, err := props(map[string]string{"model": childID, "autoload": "false"})
		if err != nil || pr.IsError() {
			continue
		}
		if n := nCtx(pr.JSON()); n > 0 {
			entry["context_length"] = n
		}
	}
}

func endpointMemoKeyFor(normalized, apiKey string) endpointMemoKey {
	fp := ""
	if apiKey != "" {
		fp = FingerprintSecretValue(apiKey)
	}
	return endpointMemoKey{url: normalized, fingerprint: fp}
}

// Cache keyed by id from an OpenAI style models payload
func parseModelsPayload(payload map[string]any) map[string]map[string]any {
	cache := map[string]map[string]any{}
	data, _ := payload["data"].([]any)
	for _, raw := range data {
		model, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		modelID := stringOf(model["id"])
		if modelID == "" {
			continue
		}
		n, has := extractFirstInt(model, contextLengthKeys)
		addModelAliases(cache, modelID, endpointModelEntry(model, modelID, n, has))
	}
	return cache
}

// FetchEndpointModelMetadata reads an OpenAI compatible models endpoint, memoized per base url and key
func FetchEndpointModelMetadata(ctx context.Context, baseURL, apiKey string, forceRefresh bool) map[string]map[string]any {
	normalized := normalizeBaseURL(baseURL)
	if normalized == "" || HostMatches(normalized, "openrouter.ai") {
		return map[string]map[string]any{}
	}
	local := IsLocalEndpoint(normalized)
	key := endpointMemoKeyFor(normalized, apiKey)
	if !forceRefresh {
		memoLock.Lock()
		cached, ok := endpointModelMetadataCache[key]
		stamp := endpointModelMetadataCacheTime[key]
		memoLock.Unlock()
		if ok && wallNow().Sub(stamp) < endpointModelCacheTTL {
			return cached
		}
		if !local {
			if memo, ok := endpointDiskCacheGet(ctx, normalized); ok {
				return rememberEndpointModels(key, memo)
			}
		}
	}
	if endpointBlackholed(normalized) {
		return map[string]map[string]any{}
	}
	alternate := normalized + "/v1"
	if strings.HasSuffix(normalized, "/v1") {
		alternate = strings.TrimRight(normalized[:len(normalized)-3], "/")
	}
	candidates := []string{normalized}
	if alternate != normalized {
		candidates = append(candidates, alternate)
	}
	headers := authHeaders(apiKey)
	var lastErr error
	if local {
		if DetectLocalServerType(ctx, normalized, apiKey) == "lm-studio" {
			cache, err := lmstudioNativeModels(ctx, normalized, headers)
			if err == nil {
				return rememberEndpointModels(key, cache)
			}
			lastErr = err
			noteIfConnectTimeout(err, normalized)
		}
	}
	client := newProbeClient(ctx, normalized, headers, 5*time.Second, 10*time.Second)
	for _, candidate := range candidates {
		if endpointBlackholed(normalized) {
			break
		}
		requestCandidate := localhostToIPv4(candidate)
		target := strings.TrimRight(requestCandidate, "/") + "/models"
		resp, err := client.Get(target, nil)
		if err == nil && (resp.StatusCode == 401 || resp.StatusCode == 403) {
			metadataLog.Debug("model metadata probe rejected, stopping candidate probing", "status", resp.StatusCode, "url", target)
			break
		}
		if err == nil {
			err = raiseForStatus(resp, target)
		}
		if err != nil {
			lastErr = err
			noteIfConnectTimeout(err, normalized)
			continue
		}
		payload := resp.JSON()
		cache := parseModelsPayload(payload)
		data, _ := payload["data"].([]any)
		for _, raw := range data {
			if m, ok := raw.(map[string]any); ok && stringOf(m["owned_by"]) == "llamacpp" {
				applyLlamacppProps(client, cache, requestCandidate)
				break
			}
		}
		if len(cache) > 0 && !local {
			endpointDiskCachePut(ctx, normalized, cache)
		}
		return rememberEndpointModels(key, cache)
	}
	if lastErr != nil {
		metadataLog.Debug("failed to fetch model metadata", "url", normalized+"/models", "error", lastErr)
	}
	return rememberEndpointModels(key, map[string]map[string]any{})
}

// Context length from an endpoint's live models metadata
func resolveEndpointContextLength(ctx context.Context, model, baseURL, apiKey string) (int, bool) {
	metadata := FetchEndpointModelMetadata(ctx, baseURL, apiKey, false)
	matched, ok := metadata[model]
	if !ok && len(metadata) == 1 {
		for _, entry := range metadata {
			matched = entry
		}
	} else if !ok && model != "" {
		for _, key := range sortedStringKeys(metadata) {
			if strings.Contains(key, model) || strings.Contains(model, key) {
				matched = metadata[key]
				break
			}
		}
	}
	if matched == nil {
		return 0, false
	}
	return intValue(matched["context_length"])
}

// Exact id or a publisher slug whose tail equals the configured name
func probeModelIDMatches(candidateID, lookupModel string) bool {
	if candidateID == lookupModel {
		return true
	}
	return strings.Contains(candidateID, "/") && lastPathSegment(candidateID) == lookupModel
}

// Ollama show payload for the model, nil on any failure
func ollamaShow(ctx context.Context, serverURL, apiKey, bareModel string, timeout time.Duration, noteBlackhole bool) map[string]any {
	client := newProbeClient(ctx, serverURL, authHeaders(apiKey), timeout, timeout)
	resp, err := client.PostJSON(serverURL+"/api/show", map[string]any{"name": bareModel})
	if err != nil {
		if noteBlackhole {
			noteIfConnectTimeout(err, serverURL)
		}
		return nil
	}
	if resp.StatusCode != 200 {
		return nil
	}
	return resp.JSON()
}

func isOllamaServer(ctx context.Context, baseURL, apiKey string) bool {
	return DetectLocalServerType(ctx, baseURL, apiKey) == "ollama"
}

// QueryOllamaNumCtx returns the Ollama show context, Modelfile num_ctx before the GGUF max
func QueryOllamaNumCtx(ctx context.Context, model, baseURL, apiKey string) (int, bool) {
	bare, server := stripProviderPrefix(model), probeServerRoot(baseURL)
	if !isOllamaServer(ctx, baseURL, apiKey) {
		return 0, false
	}
	diskKey := server + "|" + bare
	if hit, ok := intValue(localProbeDiskGet(ctx, "ollama_num_ctx", diskKey)); ok && hit > 0 {
		return hit, true
	}
	data := ollamaShow(ctx, server, apiKey, bare, 3*time.Second, false)
	if data == nil {
		return 0, false
	}
	n, ok := ollamaShowContext(data, false, 0)
	if ok {
		localProbeDiskPut(ctx, "ollama_num_ctx", diskKey, n)
	}
	return n, ok
}

// QueryOllamaSupportsVision reports Ollama vision support, known false when the server answered without it
func QueryOllamaSupportsVision(ctx context.Context, model, baseURL, apiKey string) (supported, known bool) {
	bare := stripProviderPrefix(model)
	if bare == "" || baseURL == "" || !isOllamaServer(ctx, baseURL, apiKey) {
		return false, false
	}
	data := ollamaShow(ctx, probeServerRoot(baseURL), apiKey, bare, 3*time.Second, false)
	if data == nil {
		return false, false
	}
	if caps, ok := data["capabilities"].([]any); ok && len(caps) > 0 {
		for _, cap := range caps {
			if strings.ToLower(stringOf(cap)) == "vision" {
				return true, true
			}
		}
		return false, true
	}
	if info, ok := data["model_info"].(map[string]any); ok {
		for key := range info {
			if strings.Contains(strings.ToLower(key), "vision.block_count") {
				return true, true
			}
		}
	}
	return false, false
}

type localProbeKey struct{ kind, model, baseURL string }

type localProbeHit struct {
	result int
	at     time.Time
}

// Short TTL of the positive only local probe memo
const localCtxProbeTTL = 30 * time.Second

var (
	localCtxProbeMu    sync.Mutex
	localCtxProbeCache = map[localProbeKey]localProbeHit{}
)

// Positive only short TTL memo of a local probe
func memoLocalProbe(key localProbeKey, probe func() int) int {
	now := monotonicNow()
	localCtxProbeMu.Lock()
	hit, ok := localCtxProbeCache[key]
	localCtxProbeMu.Unlock()
	if ok && now.Sub(hit.at) < localCtxProbeTTL {
		return hit.result
	}
	result := probe()
	if result > 0 {
		localCtxProbeMu.Lock()
		localCtxProbeCache[key] = localProbeHit{result: result, at: now}
		localCtxProbeMu.Unlock()
	}
	return result
}

// Provider agnostic Ollama show probe, GGUF first for hosted servers
func queryOllamaAPIShow(ctx context.Context, model, baseURL, apiKey string) int {
	key := localProbeKey{"ollama_show", stripProviderPrefix(model), strings.TrimRight(baseURL, "/")}
	return memoLocalProbe(key, func() int { return queryOllamaAPIShowUncached(ctx, model, baseURL, apiKey) })
}

func queryOllamaAPIShowUncached(ctx context.Context, model, baseURL, apiKey string) int {
	server := probeServerRoot(baseURL)
	if endpointBlackholed(server) {
		return 0
	}
	data := ollamaShow(ctx, server, apiKey, model, 5*time.Second, true)
	if data == nil {
		return 0
	}
	n, _ := ollamaShowContext(data, true, 1024)
	return n
}

// Local server context probe under the short TTL memo
func queryLocalContextLength(ctx context.Context, model, baseURL, apiKey string) int {
	key := localProbeKey{"", stripProviderPrefix(model), strings.TrimRight(baseURL, "/")}
	return memoLocalProbe(key, func() int { return queryLocalContextLengthUncached(ctx, model, baseURL, apiKey) })
}

// LM Studio native models list, loaded instance config is the runtime value
func lmstudioContext(client *probeClient, lmstudioURL, model string) (int, bool, error) {
	resp, err := client.Get(lmstudioURL+"/api/v1/models", nil)
	if err != nil {
		return 0, false, err
	}
	if resp.StatusCode != 200 {
		return 0, false, nil
	}
	models, _ := resp.JSON()["models"].([]any)
	for _, raw := range models {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if probeModelIDMatches(stringOf(m["key"]), model) || probeModelIDMatches(stringOf(m["id"]), model) {
			instances, _ := m["loaded_instances"].([]any)
			for _, rawInst := range instances {
				inst, _ := rawInst.(map[string]any)
				cfg, _ := inst["config"].(map[string]any)
				if n := positiveIntOf(cfg["context_length"]); n > 0 {
					return n, true, nil
				}
			}
			return 0, false, nil
		}
	}
	return 0, false, nil
}

// OpenAI models list match by id, else the sole model on single model servers
func openAIModelsListContext(client *probeClient, serverURL, model string) (int, bool, error) {
	resp, err := client.Get(serverURL+"/v1/models", nil)
	if err != nil {
		return 0, false, err
	}
	if resp.StatusCode != 200 {
		return 0, false, nil
	}
	list, _ := resp.JSON()["data"].([]any)
	var matched map[string]any
	for _, raw := range list {
		if m, ok := raw.(map[string]any); ok && probeModelIDMatches(stringOf(m["id"]), model) {
			matched = m
			break
		}
	}
	if matched == nil && len(list) == 1 {
		matched, _ = list[0].(map[string]any)
	}
	if matched == nil {
		return 0, false, nil
	}
	sources := []map[string]any{matched}
	if meta, ok := matched["meta"].(map[string]any); ok {
		sources = append(sources, meta)
	}
	for _, source := range sources {
		if n := positiveIntOf(source["n_ctx"]); n > 0 {
			return n, true, nil
		}
	}
	for _, source := range sources {
		if n, ok := contextLengthFromModelPayload(source); ok {
			return n, true, nil
		}
	}
	return 0, false, nil
}

// LlamacppContext reads the runtime n_ctx from props else the router launch ctx size
func LlamacppContext(client *probeClient, serverURL, model string) (int, bool, error) {
	for _, propsPath := range []string{"/props?model=" + url.QueryEscape(model) + "&autoload=false", "/props"} {
		resp, err := client.Get(serverURL+propsPath, nil)
		if err != nil {
			return 0, false, err
		}
		if resp.StatusCode != 200 {
			continue
		}
		settings, _ := resp.JSON()["default_generation_settings"].(map[string]any)
		if n := positiveIntOf(settings["n_ctx"]); n > 0 {
			return n, true, nil
		}
	}
	return RouterLaunchCtx(client, serverURL, model)
}

// RouterLaunchCtx reads the ctx size flag from the router launch args of the model
func RouterLaunchCtx(client *probeClient, serverURL, model string) (int, bool, error) {
	resp, err := client.Get(serverURL+"/models", nil)
	if err != nil {
		return 0, false, err
	}
	if resp.StatusCode != 200 {
		return 0, false, nil
	}
	entries, _ := resp.JSON()["data"].([]any)
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || !probeModelIDMatches(stringOf(entry["id"]), model) {
			continue
		}
		status, _ := entry["status"].(map[string]any)
		rawArgs, _ := status["args"].([]any)
		args := make([]string, 0, len(rawArgs))
		for _, a := range rawArgs {
			args = append(args, stringOf(a))
		}
		for i := 0; i+1 < len(args); i++ {
			flag, value := args[i], args[i+1]
			if flag == "--ctx-size" || flag == "-c" {
				if n, err := strconv.Atoi(value); err == nil && n > 0 && allDigits(value) {
					return n, true, nil
				}
			}
		}
	}
	return 0, false, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Queries a local server for the model's context length through typed then generic probes
func queryLocalContextLengthUncached(ctx context.Context, model, baseURL, apiKey string) int {
	model = stripProviderPrefix(model)
	server := probeServerRoot(baseURL)
	lmstudioURL := localhostToIPv4(lmstudioServerRoot(baseURL))
	if endpointBlackholed(server) {
		return 0
	}
	serverType := DetectLocalServerType(ctx, baseURL, apiKey)
	client := newProbeClient(ctx, baseURL, authHeaders(apiKey), 3*time.Second, 3*time.Second)
	type probe func() (int, bool, error)
	ollamaCtx := func() (int, bool, error) {
		resp, err := client.PostJSON(server+"/api/show", map[string]any{"name": model})
		if err != nil {
			return 0, false, err
		}
		if resp.StatusCode != 200 {
			return 0, false, nil
		}
		n, ok := ollamaShowContext(resp.JSON(), false, 0)
		return n, ok, nil
	}
	modelDetailCtx := func() (int, bool, error) {
		if serverType == "" {
			return 0, false, nil
		}
		resp, err := client.Get(server+"/v1/models/"+model, nil)
		if err != nil {
			return 0, false, err
		}
		if resp.StatusCode != 200 {
			return 0, false, nil
		}
		n, ok := contextLengthFromModelPayload(resp.JSON())
		return n, ok, nil
	}
	var probes []probe
	switch serverType {
	case "ollama":
		probes = append(probes, ollamaCtx)
	case "lm-studio":
		probes = append(probes, func() (int, bool, error) { return lmstudioContext(client, lmstudioURL, model) })
	case "llamacpp":
		probes = append(probes, func() (int, bool, error) { return LlamacppContext(client, server, model) })
	}
	probes = append(probes, modelDetailCtx, func() (int, bool, error) { return openAIModelsListContext(client, server, model) })
	for _, p := range probes {
		n, ok, err := p()
		if err != nil {
			noteIfConnectTimeout(err, server)
			return 0
		}
		if ok {
			return n
		}
	}
	return 0
}

// Dots to dashes so Nous ids compare with OpenRouter spellings
func normalizeModelVersion(model string) string { return strings.ReplaceAll(model, ".", "-") }

// Anthropic models max_input_tokens for console keys, OAuth tokens are skipped
func queryAnthropicContextLength(ctx context.Context, model, baseURL, apiKey string) int {
	if apiKey == "" || strings.HasPrefix(apiKey, "sk-ant-oat") {
		return 0
	}
	base := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	headers := map[string]string{"x-api-key": apiKey, "anthropic-version": "2023-06-01"}
	resp, err := newProbeClient(ctx, baseURL, headers, 5*time.Second, 10*time.Second).Get(base+"/v1/models?limit=1000", nil)
	if err != nil {
		metadataLog.Debug("Anthropic /v1/models query failed", "error", err)
		return 0
	}
	if resp.StatusCode != 200 {
		return 0
	}
	data, _ := resp.JSON()["data"].([]any)
	for _, raw := range data {
		m, ok := raw.(map[string]any)
		if !ok || stringOf(m["id"]) != model {
			continue
		}
		if n, ok := intValue(m["max_input_tokens"]); ok && n > 0 {
			return n
		}
	}
	return 0
}

// DetectLocalServerType probes known endpoints for ollama, lm-studio, vllm or llamacpp with TTL caching
func DetectLocalServerType(ctx context.Context, baseURL, apiKey string) string {
	normalized := localhostToIPv4(normalizeBaseURL(baseURL))
	if inferred := inferProviderFromBaseURL(normalized); inferred != "" && inferred != "ollama-cloud" {
		return ""
	}
	server := probeServerRoot(normalized)
	lmstudioURL := lmstudioServerRoot(normalized)
	endpointProbePathMu.Lock()
	cached, ok := endpointProbePathCache[server]
	endpointProbePathMu.Unlock()
	if ok {
		ttl := endpointProbeFailureTTL
		if cached.known {
			ttl = endpointProbeTTL
		}
		if monotonicNow().Sub(cached.at) < ttl {
			return cached.serverType
		}
	}
	if endpointBlackholed(server) {
		return ""
	}
	if hit, ok := localProbeDiskGet(ctx, "server_type", server).(string); ok {
		endpointProbePathMu.Lock()
		endpointProbePathCache[server] = serverTypeVerdict{serverType: hit, known: true, at: monotonicNow()}
		endpointProbePathMu.Unlock()
		return hit
	}
	type leg struct {
		name  string
		urls  []string
		check func(*probeResponse) bool
	}
	waterfall := []leg{
		{"lm-studio", []string{lmstudioURL + "/api/v1/models"}, func(*probeResponse) bool { return true }},
		{"ollama", []string{server + "/api/tags"}, func(r *probeResponse) bool { _, has := r.JSON()["models"]; return has }},
		{"llamacpp", []string{server + "/v1/props", server + "/props"}, func(r *probeResponse) bool { return strings.Contains(r.Text(), "default_generation_settings") }},
		{"vllm", []string{server + "/version"}, func(r *probeResponse) bool { _, has := r.JSON()["version"]; return has }},
	}
	result := ""
	client := newProbeClient(ctx, baseURL, authHeaders(apiKey), 2*time.Second, 2*time.Second)
legs:
	for _, l := range waterfall {
		var last *probeResponse
		for _, target := range l.urls {
			resp, err := client.Get(target, nil)
			if err != nil {
				if isConnectTimeout(err) {
					noteEndpointBlackholed(server)
					break legs
				}
				last = nil
				break
			}
			last = resp
			if resp.StatusCode == 200 {
				break
			}
		}
		if last != nil && last.StatusCode == 200 && l.check(last) {
			result = l.name
			break
		}
	}
	endpointProbePathMu.Lock()
	endpointProbePathCache[server] = serverTypeVerdict{serverType: result, known: result != "", at: monotonicNow()}
	endpointProbePathMu.Unlock()
	if result != "" {
		localProbeDiskPut(ctx, "server_type", server, result)
	}
	return result
}
