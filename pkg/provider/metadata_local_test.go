package provider

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestOllamaParametersNumCtx(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434/v1", "ollama")
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, map[string]any{"model_info": map[string]any{}, "parameters": "num_ctx 32768\ntemperature 0.7\n"})
	mustEqual(t, queryLocalContextLength(ctx, "some-model", "http://localhost:11434/v1", ""), 32768, "num_ctx")
}

func TestOllamaShow404FallsThrough(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434/v1", "ollama")
	ft.on("POST", "http://127.0.0.1:11434/api/show", 404, map[string]any{})
	ft.on("GET", "http://127.0.0.1:11434/v1/models/some-model", 200, map[string]any{"max_model_len": 65536})
	mustEqual(t, queryLocalContextLength(ctx, "some-model", "http://localhost:11434/v1", ""), 65536, "detail")
}

func TestVllmDetailKeys(t *testing.T) {
	for _, tc := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"id": "some-model", "max_model_len": 100000}, 100000},
		{map[string]any{"id": "some-model", "context_length": 32768}, 32768},
		{map[string]any{"type": "model", "id": "some-model", "max_input_tokens": 1000000, "max_tokens": 128000}, 1000000},
	} {
		ctx, ft, _ := metaTest(t)
		seedServerType("http://localhost:8000/v1", "vllm")
		ft.on("GET", "http://127.0.0.1:8000/v1/models/some-model", 200, tc.body)
		mustEqual(t, queryLocalContextLength(ctx, "some-model", "http://localhost:8000/v1", ""), tc.want, "detail keys")
	}
}

func TestListBranchReadsContextWindowNotOutputCap(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:8000/v1", "vllm")
	ft.on("GET", "http://127.0.0.1:8000/v1/models/some-model", 404, map[string]any{})
	ft.on("GET", "http://127.0.0.1:8000/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "some-model", "max_input_tokens": 1000000, "max_tokens": 128000}}})
	mustEqual(t, queryLocalContextLength(ctx, "some-model", "http://localhost:8000/v1", ""), 1000000, "list window")
}

func TestProbeAgreesWithTheModuleKeyVocabulary(t *testing.T) {
	keyset := map[string]bool{}
	for _, k := range contextLengthKeys {
		keyset[k] = true
	}
	found := false
	for _, k := range maxCompletionKeys {
		if keyset[k] {
			t.Fatalf("%s classified as both window and cap", k)
		}
		if k == "max_tokens" {
			found = true
		}
	}
	if !found || keyset["max_tokens"] {
		t.Fatal("max_tokens is an output cap only")
	}
	for _, k := range contextLengthKeys {
		if n, ok := extractFlatContextLength(map[string]any{k: 123456}); !ok || n != 123456 {
			t.Fatalf("%s must be honoured", k)
		}
	}
	for _, k := range maxCompletionKeys {
		if _, ok := extractFlatContextLength(map[string]any{k: 123456}); ok {
			t.Fatalf("%s must never read as a window", k)
		}
	}
}

func TestModelsListMaxModelLen(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:1234", "")
	ft.on("GET", "http://127.0.0.1:1234/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "other-model", "max_model_len": 4096}, map[string]any{"id": "omnicoder-9b", "max_model_len": 131072}}})
	mustEqual(t, queryLocalContextLength(ctx, "omnicoder-9b", "http://localhost:1234", ""), 131072, "list match")
}

func TestQueryLocalContextLengthModelsListModelsListModelNotFoundReturnsNone(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:1234", "")
	ft.on("GET", "http://127.0.0.1:1234/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "other-model", "max_model_len": 4096}, map[string]any{"id": "yet-another-model", "max_model_len": 8192}}})
	mustEqual(t, queryLocalContextLength(ctx, "omnicoder-9b", "http://localhost:1234", ""), 0, "absent")
}

func TestModelsListLlamacppMetaNCtxSoleModel(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:8080", "")
	ft.on("GET", "http://127.0.0.1:8080/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "/app/models/qwen3.6-35b.gguf", "meta": map[string]any{"n_ctx": 256000, "n_ctx_train": 262144}}}})
	mustEqual(t, queryLocalContextLength(ctx, "qwen3.6-35b", "http://localhost:8080", ""), 256000, "sole model runtime n_ctx")
	for _, u := range ft.urls() {
		if strings.Contains(u, "/v1/models/") {
			t.Fatalf("unrecognised server must skip the detail probe: %s", u)
		}
	}
}

func TestContextLengthFromModelPayload(t *testing.T) {
	if n, ok := contextLengthFromModelPayload(map[string]any{"id": "local-model", "max_model_len": 131072, "max_tokens": 4096}); !ok || n != 131072 {
		t.Fatal("window first")
	}
	if n, ok := contextLengthFromModelPayload(map[string]any{"id": "odd-server", "max_tokens": 65536}); !ok || n != 65536 {
		t.Fatal("max_tokens last resort")
	}
	if _, ok := contextLengthFromModelPayload(map[string]any{}); ok {
		t.Fatal("empty payload")
	}
	if _, ok := contextLengthFromModelPayload(nil); ok {
		t.Fatal("nil payload")
	}
}

func TestQueryLocalContextLengthAnthropicProxyModelsListPrefersMaxInputTokens(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://127.0.0.1:47821", "")
	ft.on("GET", "http://127.0.0.1:47821/v1/models", 200, map[string]any{"data": []any{
		map[string]any{"type": "model", "id": "claude-fable-5", "display_name": "Claude Fable 5", "max_input_tokens": 1_000_000, "max_tokens": 128_000},
		map[string]any{"type": "model", "id": "claude-haiku-4-5-20251001", "max_input_tokens": 200_000, "max_tokens": 64_000},
	}})
	mustEqual(t, queryLocalContextLength(ctx, "claude-fable-5", "http://127.0.0.1:47821", ""), 1_000_000, "max_input_tokens")
	ctx, ft, _ = metaTest(t)
	seedServerType("http://127.0.0.1:47821/v1", "vllm")
	ft.on("GET", "http://127.0.0.1:47821/v1/models/claude-fable-5", 200, map[string]any{"type": "model", "id": "claude-fable-5", "max_input_tokens": 1_000_000, "max_tokens": 128_000})
	mustEqual(t, queryLocalContextLength(ctx, "claude-fable-5", "http://127.0.0.1:47821/v1", ""), 1_000_000, "detail max_input_tokens")
}

func TestLmstudioExactKeyMatch(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://192.168.1.22:1234/v1", "lm-studio")
	ft.on("GET", "http://192.168.1.22:1234/api/v1/models", 200, map[string]any{"models": []any{map[string]any{"key": "nvidia/nvidia-nemotron-super-49b-v1", "id": "nvidia/nvidia-nemotron-super-49b-v1", "max_context_length": 1_048_576, "loaded_instances": []any{map[string]any{"config": map[string]any{"context_length": 131072}}}}}})
	mustEqual(t, queryLocalContextLength(ctx, "nvidia/nvidia-nemotron-super-49b-v1", "http://192.168.1.22:1234/v1", ""), 131072, "loaded instance")
}

func TestLmstudioNativeAPIBaseURLIsNotDoubled(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:1234/api/v1", "lm-studio")
	ft.on("GET", "http://127.0.0.1:1234/api/v1/models", 200, map[string]any{"models": []any{map[string]any{"key": "publisher/model-a", "id": "publisher/model-a", "loaded_instances": []any{map[string]any{"config": map[string]any{"context_length": 32768}}}}}})
	mustEqual(t, queryLocalContextLength(ctx, "publisher/model-a", "http://localhost:1234/api/v1", ""), 32768, "native")
	mustEqual(t, ft.urls()[0], "http://127.0.0.1:1234/api/v1/models", "first url")
}

func TestDetectLocalServerTypeAuthPassesBearerTokenToProbeRequests(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 200, map[string]any{}, nil), nil
	}
	mustEqual(t, DetectLocalServerType(ctx, "http://localhost:1234/v1", "lm-token"), "lm-studio", "lm studio first")
	mustEqual(t, ft.calls[0].Headers.Get("Authorization"), "Bearer lm-token", "auth header")
	ctx, ft, _ = metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 200, map[string]any{}, nil), nil
	}
	mustEqual(t, DetectLocalServerType(ctx, "http://localhost:1234/api/v1", ""), "lm-studio", "native base")
	mustEqual(t, ft.urls()[0], "http://127.0.0.1:1234/api/v1/models", "not doubled")
}

func TestDetectLocalServerTypeLocalhostIPv4LocalhostResolvedToIPv4(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 404, map[string]any{}, nil), nil
	}
	DetectLocalServerType(ctx, "http://localhost:8317/v1", "")
	for _, u := range ft.urls() {
		if strings.Contains(u, "localhost") || !strings.Contains(u, "127.0.0.1") {
			t.Fatalf("probe url still uses localhost: %s", u)
		}
	}
	ctx, ft, _ = metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 404, map[string]any{}, nil), nil
	}
	DetectLocalServerType(ctx, "http://192.168.1.100:8080", "")
	for _, u := range ft.urls() {
		if !strings.Contains(u, "192.168.1.100") {
			t.Fatalf("non localhost url changed: %s", u)
		}
	}
}

func TestDetectLocalServerTypeWaterfall(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", "http://127.0.0.1:11434/api/tags", 200, map[string]any{"models": []any{}})
	mustEqual(t, DetectLocalServerType(ctx, "http://localhost:11434/v1", ""), "ollama", "ollama")
	ctx, ft, _ = metaTest(t)
	ft.on("GET", "http://127.0.0.1:8080/v1/props", 404, map[string]any{})
	ft.onFunc("GET", "http://127.0.0.1:8080/props", func(*http.Request) *http.Response {
		return textResponse(200, `{"default_generation_settings": {"n_ctx": 4096}}`)
	})
	mustEqual(t, DetectLocalServerType(ctx, "http://localhost:8080/v1", ""), "llamacpp", "older llama.cpp without the v1 prefix")
	ctx, ft, _ = metaTest(t)
	ft.on("GET", "http://127.0.0.1:8000/version", 200, map[string]any{"version": "0.9"})
	mustEqual(t, DetectLocalServerType(ctx, "http://localhost:8000/v1", ""), "vllm", "vllm")
	mustEqual(t, DetectLocalServerType(ctx, "http://localhost:8000/v1", ""), "vllm", "cached verdict")
	mustEqual(t, ft.count(), 4, "verdict cached after one waterfall")
}

func TestFetchEndpointModelMetadataLmStudioUsesNativeModelsEndpointOnly(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:1234/v1", "lm-studio")
	ft.on("GET", "http://localhost:1234/api/v1/models", 200, map[string]any{"models": []any{map[string]any{"key": "lmstudio-community/Qwen3.5-27B-GGUF/Qwen3.5-27B-Q8_0.gguf", "id": "lmstudio-community/Qwen3.5-27B-GGUF/Qwen3.5-27B-Q8_0.gguf", "max_context_length": 1_048_576, "loaded_instances": []any{map[string]any{"config": map[string]any{"context_length": 131072}}}}}})
	got := FetchEndpointModelMetadata(ctx, "http://localhost:1234/v1", "lm-token", true)
	mustEqual(t, ft.count(), 1, "one call")
	mustEqual(t, ft.calls[0].URL, "http://localhost:1234/api/v1/models", "native url")
	mustEqual(t, ft.calls[0].Headers.Get("Authorization"), "Bearer lm-token", "auth")
	mustEqual(t, intOrDefault(got["lmstudio-community/Qwen3.5-27B-GGUF/Qwen3.5-27B-Q8_0.gguf"]["context_length"], 0), 131072, "key")
	mustEqual(t, intOrDefault(got["Qwen3.5-27B-GGUF/Qwen3.5-27B-Q8_0.gguf"]["context_length"], 0), 131072, "alias")
	ctx, ft, _ = metaTest(t)
	seedServerType("http://localhost:1234/api/v1", "lm-studio")
	ft.on("GET", "http://localhost:1234/api/v1/models", 200, map[string]any{"models": []any{map[string]any{"key": "publisher/model-a", "id": "publisher/model-a", "loaded_instances": []any{map[string]any{"config": map[string]any{"context_length": 65536}}}}}})
	got = FetchEndpointModelMetadata(ctx, "http://localhost:1234/api/v1", "", true)
	mustEqual(t, ft.calls[0].URL, "http://localhost:1234/api/v1/models", "not doubled")
	mustEqual(t, intOrDefault(got["publisher/model-a"]["context_length"], 0), 65536, "entry")
}

func TestQueryLocalContextLengthNetworkErrorConnectionErrorReturnsNone(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434/v1", "")
	ft.handle = func(r *http.Request) (*http.Response, error) { return nil, errors.New("Connection refused") }
	mustEqual(t, queryLocalContextLength(ctx, "omnicoder-9b", "http://localhost:11434/v1", ""), 0, "unreachable")
}

func TestUnrecognisedServerSkipsSingleModelProbe(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://litellm-proxy:4000/v1", "")
	ft.on("GET", "http://litellm-proxy:4000/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "my-model", "context_length": 32768}}})
	mustEqual(t, queryLocalContextLength(ctx, "my-model", "http://litellm-proxy:4000/v1", ""), 32768, "list")
	for _, u := range ft.urls() {
		if strings.Contains(u, "/v1/models/my-model") {
			t.Fatalf("detail probe fired for an unrecognised server: %s", u)
		}
	}
	ctx, ft, _ = metaTest(t)
	seedServerType("http://vllm-server:8000/v1", "vllm")
	ft.on("GET", "http://vllm-server:8000/v1/models/my-model", 200, map[string]any{"max_model_len": 65536})
	mustEqual(t, queryLocalContextLength(ctx, "my-model", "http://vllm-server:8000/v1", ""), 65536, "detail")
	if !strings.Contains(strings.Join(ft.urls(), " "), "/v1/models/my-model") {
		t.Fatal("known server type probes the single model")
	}
}

func TestLocalEndpointStaleCacheReconciledFromLiveProbe(t *testing.T) {
	ctx, ft, dir := metaTest(t)
	model, base := "NousResearch/Hermes-3-Llama-3.1-70B", "http://192.168.1.50:8000/v1"
	writeContextCacheFile(t, dir, map[string]any{model + "@" + base: 131072})
	seedServerType(base, "")
	ft.on("GET", base+"/models", 200, map[string]any{"data": []any{map[string]any{"id": model, "max_model_len": 32768}}})
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: model, BaseURL: base, Provider: "custom"}), 32768, "live wins")
	if _, ok := readContextCacheFile(t, dir)[model+"@"+base]; ok {
		t.Fatal("stale entry invalidated and sub minimum window not persisted")
	}
}

func TestGetModelContextLengthLocalFallbackLocalEndpointServerReturnsNoneFallsBackTo2m(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "omnicoder-9b", BaseURL: "http://localhost:11434/v1"}), ContextProbeTiers[0], "fallback")
}

func TestCachedResultSkipsLocalQuery(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	SaveContextLength(ctx, "omnicoder-9b", "https://api.example.com/v1", 65536, "")
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "omnicoder-9b", BaseURL: "https://api.example.com/v1"}), 65536, "cached")
	mustEqual(t, ft.count(), 0, "no probe")
}

func TestLocalContextProbeTTLCache(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434/v1", "ollama")
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, map[string]any{"model_info": map[string]any{"llama.context_length": 32768}})
	mustEqual(t, queryLocalContextLength(ctx, "m", "http://localhost:11434/v1", ""), 32768, "first")
	mustEqual(t, queryLocalContextLength(ctx, "m", "http://localhost:11434/v1", ""), 32768, "second")
	mustEqual(t, ft.count(), 1, "served from memo")
	queryLocalContextLength(ctx, "m2", "http://localhost:11434/v1", "")
	mustEqual(t, ft.count(), 2, "different key probes")
	ctx, ft, _ = metaTest(t)
	seedServerType("http://localhost:11434/v1", "")
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 404, map[string]any{}, nil), nil
	}
	mustEqual(t, queryLocalContextLength(ctx, "m", "http://localhost:11434/v1", ""), 0, "miss")
	first := ft.count()
	mustEqual(t, queryLocalContextLength(ctx, "m", "http://localhost:11434/v1", ""), 0, "retry")
	if ft.count() <= first {
		t.Fatal("a failed probe must not be memoized")
	}
}

func TestModelsListMaxTokensOnlyFallsBack(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://127.0.0.1:8080/v1", "")
	ft.on("GET", "http://127.0.0.1:8080/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "mystery-model", "max_tokens": 393216}}})
	mustEqual(t, queryLocalContextLength(ctx, "mystery-model", "http://127.0.0.1:8080/v1", ""), 393216, "last resort")
}

func TestPoisonedCacheEntryRewrittenUpward(t *testing.T) {
	ctx, ft, dir := metaTest(t)
	model, base := "deepseek-v4-flash", "http://127.0.0.1:8080/v1"
	writeContextCacheFile(t, dir, map[string]any{model + "@" + base: 393216})
	seedServerType(base, "")
	ft.on("GET", base+"/models", 200, map[string]any{"data": []any{map[string]any{"id": model, "context_size": 1048576, "max_tokens": 393216}}})
	mustEqual(t, reconcileLocalCachedContextLength(ctx, model, base, 393216, ""), 1048576, "healed")
	mustEqual(t, intOrDefault(readContextCacheFile(t, dir)[model+"@"+base], 0), 1048576, "persisted upward")
}

func TestPublicHostsGetNoProbeLocalHostsDo(t *testing.T) {
	for _, tc := range []struct {
		baseURL string
		probes  bool
	}{{"https://api.openai.com/v1", false}, {"https://api.openai.com./v1", false}, {"https://api.anthropic.com", false}, {"http://127.0.0.1:11434/v1", true}, {"http://my-box:8080/v1", true}} {
		ctx, ft, _ := metaTest(t)
		ft.handle = func(r *http.Request) (*http.Response, error) {
			return jsonResponse(nil, 404, map[string]any{}, nil), nil
		}
		mustEqual(t, DetectLocalServerType(ctx, tc.baseURL, ""), "", tc.baseURL)
		mustEqual(t, ft.count() > 0, tc.probes, tc.baseURL+" probes")
	}
}

func TestQueryOllamaNumCtxAndVision(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434", "ollama")
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, map[string]any{"parameters": "num_ctx 8192", "model_info": map[string]any{"llama.context_length": 131072, "llama.vision.block_count": 32}})
	n, ok := QueryOllamaNumCtx(ctx, "ollama:llava", "http://localhost:11434", "")
	if !ok || n != 8192 {
		t.Fatalf("num_ctx got %d %v", n, ok)
	}
	n, ok = QueryOllamaNumCtx(ctx, "ollama:llava", "http://localhost:11434", "")
	if !ok || n != 8192 || ft.count() != 1 {
		t.Fatal("disk memo answers the second call")
	}
	supported, known := QueryOllamaSupportsVision(ctx, "llava", "http://localhost:11434", "")
	if !supported || !known {
		t.Fatal("vision block count proves vision")
	}
	ctx, ft, _ = metaTest(t)
	seedServerType("http://localhost:11434", "ollama")
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, map[string]any{"capabilities": []any{"completion"}})
	supported, known = QueryOllamaSupportsVision(ctx, "llama3", "http://localhost:11434", "")
	if supported || !known {
		t.Fatal("capabilities without vision is a known false")
	}
	if _, known := QueryOllamaSupportsVision(ctx, "llama3", "https://api.openai.com/v1", ""); known {
		t.Fatal("hosted provider is not ollama")
	}
}

func TestLlamacppContextAndRouterLaunch(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	client := newProbeClient(ctx, "http://127.0.0.1:8080", nil, 0, 0)
	ft.on("GET", "http://127.0.0.1:8080/props?model=qwen&autoload=false", 400, map[string]any{})
	ft.on("GET", "http://127.0.0.1:8080/props", 400, map[string]any{})
	ft.on("GET", "http://127.0.0.1:8080/models", 200, map[string]any{"data": []any{map[string]any{"id": "qwen", "status": map[string]any{"args": []any{"llama-server", "--ctx-size", "65536"}}}}})
	n, ok, err := LlamacppContext(client, "http://127.0.0.1:8080", "qwen")
	if err != nil || !ok || n != 65536 {
		t.Fatalf("router launch ctx: %d %v %v", n, ok, err)
	}
	ctx, ft, _ = metaTest(t)
	client = newProbeClient(ctx, "http://127.0.0.1:8080", nil, 0, 0)
	ft.on("GET", "http://127.0.0.1:8080/props?model=qwen&autoload=false", 200, map[string]any{"default_generation_settings": map[string]any{"n_ctx": 32768}})
	n, ok, _ = LlamacppContext(client, "http://127.0.0.1:8080", "qwen")
	if !ok || n != 32768 {
		t.Fatal("loaded model props")
	}
}
