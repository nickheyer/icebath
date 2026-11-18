package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Transport seam stand in answering catalog requests from a function
type modelsFakeTransport func(req *http.Request) (*http.Response, error)

func (f modelsFakeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func modelsJSONResponse(req *http.Request, status int, payload any) *http.Response {
	raw, _ := json.Marshal(payload)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: req}
}

// Temp home bound through the context plus every process cache reset
func modelsTestHome(t *testing.T) context.Context {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(home.EnvHome, dir)
	ctx := home.WithOverride(context.Background(), dir)
	ResetModelsFetchCaches()
	ResetProviderModelsCaches()
	ResetCatalogCache()
	bindModelsSeams()
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("network disabled in tests: " + req.URL.String())
	})
	modelsClock = time.Now
	t.Cleanup(func() {
		modelsTransport = nil
		modelsClock = time.Now
		bindModelsSeams()
		ResetModelsFetchCaches()
		ResetProviderModelsCaches()
	})
	return ctx
}

func modelsIDs(rows []CatalogModel) []string { return CatalogModelIDs(rows) }

func modelsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFetchOpenRouterModelsFallsBackToStaticSnapshotOnFetchFailure(t *testing.T) {
	ctx := modelsTestHome(t)
	curatedOpenRouterModelsFn = func(context.Context) []CatalogModel { return nil }
	models := FetchOpenRouterModels(ctx, FetchModelsOptions{ForceRefresh: true})
	if !modelsEqual(modelsIDs(models), modelsIDs(OpenRouterModels)) {
		t.Fatalf("expected the static snapshot, got %v", models)
	}
}

func TestFetchOpenRouterModelsFiltersOutModelsWithoutToolSupport(t *testing.T) {
	ctx := modelsTestHome(t)
	curatedOpenRouterModelsFn = func(context.Context) []CatalogModel {
		return []CatalogModel{{ID: "anthropic/claude-opus-4.6"}, {ID: "google/gemini-3-pro-image-preview"}, {ID: "qwen/qwen3.7-max"}}
	}
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		return modelsJSONResponse(req, 200, map[string]any{"data": []any{
			map[string]any{"id": "anthropic/claude-opus-4.6", "pricing": map[string]any{"prompt": "0.000015", "completion": "0.000075"}, "supported_parameters": []any{"temperature", "tools", "tool_choice"}},
			map[string]any{"id": "google/gemini-3-pro-image-preview", "pricing": map[string]any{"prompt": "0.00001", "completion": "0.00003"}, "supported_parameters": []any{"temperature", "response_format"}},
			map[string]any{"id": "qwen/qwen3.7-max", "pricing": map[string]any{"prompt": "0.000000325", "completion": "0.00000195"}, "supported_parameters": []any{"tools", "temperature"}},
		}}), nil
	})
	ids := modelsIDs(FetchOpenRouterModels(ctx, FetchModelsOptions{ForceRefresh: true}))
	if !containsString(ids, "anthropic/claude-opus-4.6") || !containsString(ids, "qwen/qwen3.7-max") {
		t.Fatalf("tool capable models missing: %v", ids)
	}
	if containsString(ids, "google/gemini-3-pro-image-preview") {
		t.Fatalf("image only model must be dropped: %v", ids)
	}
}

func TestOpenRouterToolSupportHelper(t *testing.T) {
	if openrouterModelSupportsTools(map[string]any{"id": "x", "supported_parameters": []any{}}) {
		t.Fatal("explicit empty list means no tools")
	}
	if !openrouterModelSupportsTools(map[string]any{"id": "x"}) {
		t.Fatal("absent field is permissive")
	}
	if !openrouterModelSupportsTools(map[string]any{"id": "x", "supported_parameters": "bad"}) {
		t.Fatal("malformed field is permissive")
	}
}

func TestOpenRouterCatalogDiskCache(t *testing.T) {
	ctx := modelsTestHome(t)
	var calls []string
	payload := []any{map[string]any{"id": "a/one", "supported_parameters": []any{"tools"}, "pricing": map[string]any{"prompt": "0", "completion": "0"}}}
	fetchLiveCatalogIndexFn = func(_ context.Context, rawURL string, _ time.Duration) ([]any, map[string]map[string]any, bool) {
		calls = append(calls, rawURL)
		return payload, map[string]map[string]any{"a/one": payload[0].(map[string]any)}, true
	}
	curatedOpenRouterModelsFn = func(context.Context) []CatalogModel { return []CatalogModel{{ID: "a/one"}} }

	first := FetchOpenRouterModels(ctx, FetchModelsOptions{})
	if len(calls) == 0 {
		t.Fatal("cold process must fetch")
	}
	if _, err := os.Stat(openrouterCatalogDiskPath(ctx)); err != nil {
		t.Fatalf("disk snapshot missing: %v", err)
	}
	openrouterCatalogSlot.reset()
	calls = nil
	again := FetchOpenRouterModels(ctx, FetchModelsOptions{})
	if len(calls) != 0 || !modelsEqual(modelsIDs(again), modelsIDs(first)) {
		t.Fatalf("fresh snapshot must serve a cold process without network: calls=%v rows=%v", calls, again)
	}

	openrouterCatalogSlot.reset()
	path := openrouterCatalogDiskPath(ctx)
	stale := float64(time.Now().Unix()) - openrouterCatalogDiskTTL(ctx).Seconds() - 1
	raw, _ := json.Marshal(map[string]any{"fetched_at": stale, "curated": [][]string{{"old/model", ""}}})
	os.WriteFile(path, raw, 0o600)
	calls = nil
	rows := FetchOpenRouterModels(ctx, FetchModelsOptions{})
	if len(rows) != 1 || rows[0] != (CatalogModel{ID: "a/one", Description: "free"}) || len(calls) != 1 {
		t.Fatalf("expired snapshot must refetch: rows=%v calls=%v", rows, calls)
	}
	openrouterCatalogSlot.reset()
	os.WriteFile(path, []byte("{not json"), 0o600)
	rows = FetchOpenRouterModels(ctx, FetchModelsOptions{})
	if len(rows) != 1 || rows[0] != (CatalogModel{ID: "a/one", Description: "free"}) || len(calls) != 2 {
		t.Fatalf("corrupt snapshot must refetch: rows=%v calls=%v", rows, calls)
	}
}

var (
	modelsPaid = map[string]any{"prompt": "0.000003", "completion": "0.000015"}
	modelsFree = map[string]any{"prompt": "0", "completion": "0"}
)

func modelsPortalPayload(key string, ids ...string) map[string]any {
	entries := make([]any, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, map[string]any{"modelName": id, "displayName": id})
	}
	return map[string]any{key: entries}
}

func TestAIGatewayFreeDetector(t *testing.T) {
	cases := []struct {
		pricing any
		want    bool
	}{
		{map[string]any{"input": "0", "output": "0"}, true},
		{map[string]any{"input": "0", "output": "0.01"}, false},
		{map[string]any{"input": "0.01", "output": "0"}, false},
		{nil, false},
		{map[string]any{"input": "not a number"}, false},
	}
	for _, c := range cases {
		if got := aiGatewayModelIsFree(c.pricing); got != c.want {
			t.Fatalf("%v: got %v want %v", c.pricing, got, c.want)
		}
	}
}

func modelsAIGatewayTransport(payload map[string]any) modelsFakeTransport {
	return func(req *http.Request) (*http.Response, error) { return modelsJSONResponse(req, 200, payload), nil }
}

func TestFetchAIGatewayModelsFiltersAgainstLiveCatalog(t *testing.T) {
	ctx := modelsTestHome(t)
	preferred := modelsIDs(VercelAIGatewayModels)
	live := preferred[:3]
	var data []any
	for _, mid := range live {
		data = append(data, map[string]any{"id": mid, "pricing": map[string]any{"input": "0.001", "output": "0.002"}})
	}
	modelsTransport = modelsAIGatewayTransport(map[string]any{"data": data})
	result := FetchAIGatewayModels(ctx, 0, true)
	if !modelsEqual(modelsIDs(result), live) || result[0].Description != "recommended" {
		t.Fatalf("live filtered rows expected: %v", result)
	}
}

func TestFetchAIGatewayModelsTagsFreeModelsAndPromotesFreeMoonshot(t *testing.T) {
	ctx := modelsTestHome(t)
	first, second := VercelAIGatewayModels[0].ID, VercelAIGatewayModels[1].ID
	modelsTransport = modelsAIGatewayTransport(map[string]any{"data": []any{
		map[string]any{"id": first, "pricing": map[string]any{"input": "0.001", "output": "0.002"}},
		map[string]any{"id": second, "pricing": map[string]any{"input": "0", "output": "0"}},
	}})
	result := FetchAIGatewayModels(ctx, 0, true)
	byID := map[string]string{}
	for _, r := range result {
		byID[r.ID] = r.Description
	}
	if byID[first] != "recommended" || byID[second] != "free" {
		t.Fatalf("free tagging: %v", result)
	}
	modelsTransport = modelsAIGatewayTransport(map[string]any{"data": []any{
		map[string]any{"id": first, "pricing": map[string]any{"input": "0.001", "output": "0.002"}},
		map[string]any{"id": "moonshotai/kimi-coder-free-preview", "pricing": map[string]any{"input": "0", "output": "0"}},
	}})
	result = FetchAIGatewayModels(ctx, 0, true)
	if result[0] != (CatalogModel{ID: "moonshotai/kimi-coder-free-preview", Description: "recommended"}) || !containsString(modelsIDs(result), first) {
		t.Fatalf("free moonshot promoted to the top: %v", result)
	}
	modelsTransport = modelsAIGatewayTransport(map[string]any{"data": []any{
		map[string]any{"id": first, "pricing": map[string]any{"input": "0.001", "output": "0.002"}},
		map[string]any{"id": "moonshotai/some-paid-variant", "pricing": map[string]any{"input": "0.001", "output": "0.002"}},
	}})
	if result = FetchAIGatewayModels(ctx, 0, true); result[0].ID != first {
		t.Fatalf("paid moonshot is not promoted: %v", result)
	}
	ResetModelsFetchCaches()
	modelsTransport = modelsFakeTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("network") })
	if result = FetchAIGatewayModels(ctx, 0, true); !modelsEqual(modelsIDs(result), modelsIDs(VercelAIGatewayModels)) {
		t.Fatalf("fetch failure falls back to the curated list: %v", result)
	}
}

func TestFetchAIGatewayModelsLiveRoutesBearerThroughCatalogClient(t *testing.T) {
	ctx := modelsTestHome(t)
	t.Setenv("AI_GATEWAY_API_KEY", "gw-secret")
	var auth string
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		auth = req.Header.Get("Authorization")
		return modelsJSONResponse(req, 200, map[string]any{"data": []any{}}), nil
	})
	ids, ok := fetchAIGatewayModelsLive(ctx, 0)
	if !ok || len(ids) != 0 || auth != "Bearer gw-secret" {
		t.Fatalf("bearer must ride the catalog client: ok=%v ids=%v auth=%q", ok, ids, auth)
	}
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		return modelsJSONResponse(req, 200, map[string]any{"data": []any{
			map[string]any{"id": "a/chat", "type": "language", "tags": []any{"tool-use"}},
			map[string]any{"id": "a/embed", "type": "embedding", "tags": []any{"tool-use"}},
			map[string]any{"id": "a/notools", "type": "language", "tags": []any{}},
		}}), nil
	})
	if ids, _ = fetchAIGatewayModelsLive(ctx, 0); !modelsEqual(ids, []string{"a/chat"}) {
		t.Fatalf("language models with tool use only: %v", ids)
	}
}

func TestModelsGetJSONDropsCredentialsAcrossOrigins(t *testing.T) {
	ctx := modelsTestHome(t)
	var hops []string
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		hops = append(hops, req.URL.Host+" "+req.Header.Get("Authorization")+" "+req.Header.Get("X-Tenant"))
		if req.URL.Host == "a.example" {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://b.example/models"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		return modelsJSONResponse(req, 200, map[string]any{"ok": true}), nil
	})
	_, err := modelsGetJSON(ctx, "https://a.example/models", time.Second, map[string]string{"Authorization": "Bearer k", "X-Tenant": "t", "Accept": "application/json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 || hops[0] != "a.example Bearer k t" || hops[1] != "b.example  " {
		t.Fatalf("private headers must not cross origins: %v", hops)
	}
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":"no"}`)), Request: req}, nil
	})
	_, err = modelsGetJSON(ctx, "https://a.example/models", time.Second, nil, nil)
	var httpErr *modelsHTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 401 || string(httpErr.Body) != `{"error":"no"}` {
		t.Fatalf("non 2xx surfaces as modelsHTTPError: %v", err)
	}
}

func TestDeepinfraTaggedCatalog(t *testing.T) {
	ctx := modelsTestHome(t)
	t.Setenv("DEEPINFRA_API_KEY", "di-key")
	var calls int32
	var auth, path string
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		auth, path = req.Header.Get("Authorization"), req.URL.Path+"?"+req.URL.RawQuery
		return modelsJSONResponse(req, 200, map[string]any{"data": []any{
			map[string]any{"id": "org/chat-model", "metadata": map[string]any{"tags": []any{"chat", "reasoning"}}},
			map[string]any{"id": "org/embed-model", "metadata": map[string]any{"tags": []any{"embed"}}},
			map[string]any{"id": "org/untagged-model", "metadata": map[string]any{"tags": []any{"vision"}}},
			map[string]any{"id": "org/flux-schnell", "metadata": map[string]any{}},
			map[string]any{"id": "org/stub", "metadata": nil},
			map[string]any{"id": "org/img", "metadata": map[string]any{"tags": []any{"image-gen"}}},
		}}), nil
	})
	chat := DeepinfraModelIDs(ctx, "chat", false)
	if !modelsEqual(chat, []string{"org/chat-model", "org/untagged-model"}) {
		t.Fatalf("chat surface: %v", chat)
	}
	if auth != "Bearer di-key" || path != "/v1/openai/models?"+deepinfraModelsQuery {
		t.Fatalf("request shape: %q %q", auth, path)
	}
	if got := DeepinfraModelIDs(ctx, "image-gen", false); !modelsEqual(got, []string{"org/img"}) || calls != 1 {
		t.Fatalf("one round trip serves every surface: %v calls=%d", got, calls)
	}
	key, rawURL := deepinfraCatalogURLIn(ctx)
	if !strings.HasPrefix(key, DeepinfraDefaultBaseURL+"#sha256:") || rawURL != DeepinfraDefaultBaseURL+"/models?"+deepinfraModelsQuery || !deepinfraCatalogCached(key) {
		t.Fatalf("cache key carries the key fingerprint: %q %q", key, rawURL)
	}
	if fetchDeepinfraModelsByTag("chat", time.Second, false) == nil {
		t.Fatal("seam returns the cached items")
	}
	modelsTransport = modelsFakeTransport(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("down")
	})
	if got := DeepinfraModelIDs(ctx, "chat", false); !modelsEqual(got, chat) {
		t.Fatalf("a resident catalog keeps serving: %v", got)
	}
	ResetModelsFetchCaches()
	calls = 0
	ids, ok := fetchDeepinfraModels(ctx, 0, false)
	if ok || ids != nil || calls != 1 {
		t.Fatalf("network failure is nil: %v %v calls=%d", ids, ok, calls)
	}
	if got := DeepinfraModelIDs(ctx, "chat", false); len(got) != 0 || calls != 1 {
		t.Fatalf("negative cache suppresses the refetch: %v calls=%d", got, calls)
	}
	if got := DeepinfraBaseURL(ctx, map[string]any{"base_url": "https://proxy.example/v1/"}); got != "https://proxy.example/v1" {
		t.Fatalf("section base url wins: %q", got)
	}
	if got := DeepinfraBaseURL(ctx, nil); got != DeepinfraDefaultBaseURL {
		t.Fatalf("default base url: %q", got)
	}
}

func TestClampReasoningEffortToSupported(t *testing.T) {
	cases := []struct {
		effort    string
		supported []string
		want      string
	}{
		{"high", []string{"low", "medium", "high"}, "high"},
		{"ultra", nil, "ultra"},
		{"ultra", []string{}, "ultra"},
		{"ultra", []string{"low", "medium", "high"}, "high"},
		{"xhigh", []string{"low", "medium", "high"}, "high"},
		{"max", []string{"minimal", "low"}, "low"},
		{"minimal", []string{"low", "medium"}, "low"},
		{"none", []string{"low", "high"}, "low"},
		{"turbo-think", []string{"low", "high"}, "turbo-think"},
		{"high", []string{"banana"}, "high"},
		{"", []string{"low"}, ""},
		{"medium", []string{"low", "xhigh"}, "low"},
	}
	for _, c := range cases {
		if got := ClampReasoningEffortToSupported(c.effort, c.supported); got != c.want {
			t.Fatalf("%q %v: got %q want %q", c.effort, c.supported, got, c.want)
		}
	}
}
