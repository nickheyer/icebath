package provider

import (
	"context"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestOllamaProbeCacheKeyBlake2b(t *testing.T) {
	if got := hex.EncodeToString(blake2bSum([]byte("abc"), 8)); got != "d8bb14d833d59559" {
		t.Fatalf("abc %s", got)
	}
	if got := hex.EncodeToString(blake2bSum([]byte(strings.Repeat("x", 300)), 8)); got != "12b539c1cad94615" {
		t.Fatalf("multi block %s", got)
	}
	key := ollamaProbeCacheKey("http://h:11434", map[string]string{"X-A": "b", "Authorization": "Bearer k"})
	if key != "http://h:11434|headers:13c0ddaa19f31909" {
		t.Fatalf("key %s", key)
	}
	if ollamaProbeCacheKey("http://h:11434", nil) != "http://h:11434" {
		t.Fatal("headerless key")
	}
}

func TestOllamaHostAndRootRules(t *testing.T) {
	cases := map[string]string{
		":11500": "127.0.0.1:11500", "[::1]": "[::1]:11434", "myhost": "myhost:11434",
		"fe80::1": "[fe80::1]:11434", "http://box": "http://box:11434", "http://u:p@box": "http://u:p@box:11434",
		"http://box:9000": "http://box:9000", "box:9000": "box:9000",
	}
	for in, want := range cases {
		if got := ollamaHostFromEnv(in); got != want {
			t.Fatalf("%s -> %s want %s", in, got, want)
		}
	}
	if got := rootForOllamaNativeAPI("localhost:11434/v1/"); got != "http://localhost:11434" {
		t.Fatalf("root %s", got)
	}
	if !sameOllamaNativeRoot("http://LOCALHOST:11434/v1", "http://localhost:11434/api") || sameOllamaNativeRoot("http://a:11434", "http://b:11434") {
		t.Fatal("same root")
	}
	if got := normalizeOpenAIBaseURL(":8080/v1"); got != "http://127.0.0.1:8080/v1" {
		t.Fatalf("openai base %s", got)
	}
}

func TestOllamaProbeCachesSuccessFailureAndReachability(t *testing.T) {
	ctx := validateTestHome(t)
	calls := 0
	fail := false
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Path != "/api/tags" {
			t.Fatalf("path %s", req.URL.Path)
		}
		if fail {
			return jsonResponse(req, 500, map[string]any{}, nil), nil
		}
		return jsonResponse(req, 200, jsonMap(`{"models":[{"model":"llama3:8b"},{"name":"qwen3"},{"model":"llama3:8b"}]}`), nil), nil
	})
	models, ok := ProbeOllamaLocalModels(ctx, "http://127.0.0.1:11434/v1", 0, nil)
	if !ok || strings.Join(models, ",") != "llama3:8b,qwen3" {
		t.Fatalf("models %v %v", models, ok)
	}
	if _, ok := ProbeOllamaLocalModels(ctx, "http://127.0.0.1:11434", 0, nil); !ok || calls != 1 {
		t.Fatalf("cache miss, calls %d", calls)
	}
	if !shouldUseOllamaNativeCatalog(ctx, "custom", "http://127.0.0.1:11434/v1", nil) || shouldUseOllamaNativeCatalog(ctx, "openrouter", "http://127.0.0.1:11434", nil) {
		t.Fatal("native catalog gate")
	}
	if shouldUseOllamaNativeCatalog(ctx, "ollama", "https://ollama.com/v1", nil) {
		t.Fatal("ollama cloud host used the native catalog")
	}
	fail = true
	if _, ok := ProbeOllamaLocalModels(ctx, "http://127.0.0.1:11999", 0, nil); ok {
		t.Fatal("failed probe answered")
	}
	before := calls
	if _, ok := ProbeOllamaLocalModels(ctx, "http://127.0.0.1:11999", 0, nil); ok || calls != before {
		t.Fatal("negative cache not honoured")
	}
	clearOllamaLocalCaches()
	fail = false
	if _, ok := ProbeOllamaLocalModels(ctx, "http://127.0.0.1:11999", 0, nil); !ok {
		t.Fatal("clear did not drop the negative entry")
	}
}

func TestOllamaLocalCatalogAndReachability(t *testing.T) {
	ctx := validateTestHome(t)
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(req, 200, jsonMap(`{"models":[]}`), nil), nil
	})
	t.Setenv("OLLAMA_HOST", "127.0.0.1")
	if got := ollamaLocalCatalog(ctx, true); got == nil || len(got) != 0 {
		t.Fatalf("empty reachable catalog %v", got)
	}
	if !ollamaNativeProbeReachable(ctx) {
		t.Fatal("reachable root not recorded")
	}
	clearOllamaLocalCaches()
	if ollamaNativeProbeReachable(ctx) {
		t.Fatal("clear kept reachability")
	}
}

func TestOllamaCloudModelsCacheLadder(t *testing.T) {
	ctx := validateTestHome(t)
	t.Setenv("OLLAMA_API_KEY", "ok")
	live := []string{"kimi-k2.6", "glm-5"}
	fetchAPIModelsFn = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration, apiMode string, headers map[string]string) ([]string, bool) {
		if baseURL != "https://ollama.com/v1" || timeout != 8*time.Second {
			t.Fatalf("fetch %s %v", baseURL, timeout)
		}
		return cloneStrings(live), live != nil
	}
	listAgenticModelsFn = func(ctx context.Context, provider string, allowNetwork bool) []string {
		return []string{"kimi-k2.6:cloud", "qwen3-coder-cloud"}
	}
	got := FetchOllamaCloudModels(ctx, OllamaCloudFetch{})
	if strings.Join(got, ",") != "kimi-k2.6,glm-5,qwen3-coder" {
		t.Fatalf("merged %v", got)
	}
	live = nil
	if got := FetchOllamaCloudModels(ctx, OllamaCloudFetch{}); strings.Join(got, ",") != "kimi-k2.6,glm-5,qwen3-coder" {
		t.Fatalf("fresh cache %v", got)
	}
	if got := FetchOllamaCloudModels(ctx, OllamaCloudFetch{ForceRefresh: true}); strings.Join(got, ",") != "kimi-k2.6,glm-5,qwen3-coder" {
		t.Fatalf("stale fallback %v", got)
	}
	os.Remove(filepath.Join(home.Dir(ctx), "ollama_cloud_models_cache.json"))
	if got, ok := fetchOllamaCloudModels(ctx, true); !ok || strings.Join(got, ",") != "kimi-k2.6,qwen3-coder" {
		t.Fatalf("models.dev only %v", got)
	}
	if _, err := os.Stat(filepath.Join(home.Dir(ctx), "ollama_cloud_models_cache.json")); err == nil {
		t.Fatal("models.dev only list was persisted")
	}
}

func TestOllamaLMStudioLoadAndReasoning(t *testing.T) {
	ctx := validateTestHome(t)
	var posted string
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			buf := make([]byte, 512)
			n, _ := req.Body.Read(buf)
			posted = string(buf[:n])
			return jsonResponse(req, 200, jsonMap(`{"load_config":{"context_length":8192}}`), nil), nil
		}
		return jsonResponse(req, 200, jsonMap(`{"models":[{"key":"m","max_context_length":32768,"loaded_instances":[],"capabilities":{"reasoning":{"allowed_options":["Low","HIGH",3]}}}]}`), nil), nil
	})
	if got := LMStudioModelReasoningOptions(ctx, "m", "http://localhost:1234/v1", nil, 0); strings.Join(got, ",") != "low,high" {
		t.Fatalf("reasoning %v", got)
	}
	r := EnsureLMStudioModelLoaded(ctx, "m", "http://localhost:1234/v1", nil, 8192, true, 0)
	if r.ContextLength != 8192 || !r.LoadAttempted || posted != `{"model":"m","echo_load_config":true,"context_length":8192}` {
		t.Fatalf("load %+v %s", r, posted)
	}
	if r := EnsureLMStudioModelLoaded(ctx, "m", "http://localhost:1234/v1", nil, 65536, true, 0); !r.Rejected || r.LoadAttempted {
		t.Fatalf("over max %+v", r)
	}
	if r := EnsureLMStudioModelLoaded(ctx, "m", "http://localhost:1234/v1", nil, 0, true, 0); r != (LMStudioLoadResult{}) {
		t.Fatalf("non positive target %+v", r)
	}
}
