package provider

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNvidiaDeepseekV4ProContextIsEndpointScoped(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	for _, baseURL := range []string{"https://integrate.api.nvidia.com/v1", "https://INTEGRATE.API.NVIDIA.COM/v1/", "https://integrate.api.nvidia.com:443/v1"} {
		mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "deepseek-ai/deepseek-v4-pro", Provider: "nvidia", BaseURL: baseURL}), 262_144, baseURL)
	}
	for _, baseURL := range []string{"http://integrate.api.nvidia.com/v1", "https://integrate.api.nvidia.com:8443/v1", "https://integrate.api.nvidia.com/v1/other", "https://integrate.api.nvidia.com/v1?route=other", "https://example.invalid/v1", "https://api.deepseek.com/v1", "https://openrouter.ai/api/v1"} {
		mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "deepseek-ai/deepseek-v4-pro", Provider: "nvidia", BaseURL: baseURL}), 1_000_000, baseURL)
	}
}

func TestK3ContextIsScopedToConfirmedCodingEndpoint(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	for _, baseURL := range []string{"https://api.kimi.com/coding", "https://API.KIMI.COM/coding/", "https://api.kimi.com:443/coding", "https://api.kimi.com/coding/v1"} {
		for _, model := range []string{"k3", "kimi-k3", "kimi-k3-cot"} {
			mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: model, Provider: "kimi-coding", BaseURL: baseURL}), 1_048_576, model+"@"+baseURL)
		}
	}
	for _, baseURL := range []string{"http://api.kimi.com/coding", "https://api.kimi.com:8443/coding", "https://api.kimi.com/coding/../other", "https://api.kimi.com/codingevil", "https://example.invalid/coding", "https://[api.kimi.com/coding", "https://api.moonshot.ai/v1", "https://api.moonshot.cn/v1"} {
		if got := GetModelContextLength(ctx, ContextLengthRequest{Model: "k3", Provider: "kimi-coding", BaseURL: baseURL}); got == 1_048_576 {
			t.Fatalf("bare k3 leaked its scoped window to %s", baseURL)
		}
		for _, model := range []string{"kimi-k3", "kimi-k3-cot"} {
			mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: model, Provider: "kimi-coding", BaseURL: baseURL}), 1_048_576, model+"@"+baseURL)
		}
	}
}

func upstageCtx(t *testing.T, model string) int {
	t.Helper()
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	return GetModelContextLength(ctx, ContextLengthRequest{Model: model, Provider: "upstage", BaseURL: "https://api.upstage.ai/v1"})
}

func TestUpstageSolarIDsMatchLegacyKeysOnlyOnAnIDBoundary(t *testing.T) {
	family := DefaultContextLengths()["solar-"]
	if family <= DefaultFallbackContext {
		t.Fatal("family window must exceed the fallback")
	}
	for _, model := range []string{"solar-mini4", "solar-mini4-preview", "upstage/solar-mini4", "solar-pro4", "solar-pro4-260806", "solar-pro4-quant", "solar-mini12", "solar-foo"} {
		mustEqual(t, upstageCtx(t, model), family, model)
		hit, ok := longestKeyMatch(defaultContextLengths, strings.ToLower(model))
		if !ok || hit.ctx != family {
			t.Fatalf("%s: table miss must agree with the resolver", model)
		}
	}
	for _, pair := range [][2]string{{"solar-mini-250422", "solar-mini"}, {"upstage/solar-mini", "solar-mini"}, {"solar-pro2-251215", "solar-pro2"}, {"solar-pro3-260323", "solar-pro3"}, {"upstage/solar-pro-3", "solar-pro3"}, {"solar-pro3.1", "solar-pro3"}, {"solar-open2@q4_k_m", "solar-open2"}} {
		mustEqual(t, upstageCtx(t, pair[0]), upstageCtx(t, pair[1]), pair[0])
	}
	if upstageCtx(t, "solar-mini") >= MinimumContextLength {
		t.Fatal("solar-mini keeps its 32K window")
	}
	if upstageCtx(t, "solar-pro3") >= family {
		t.Fatal("solar-pro3 keeps its own window")
	}
	for _, model := range []string{"ft:solar-news-correction", "acme-solar-foo", "upstage/solar-10.7b-instruct"} {
		if upstageCtx(t, model) == family {
			t.Fatalf("%s is not a Solar API lineup", model)
		}
	}
}

func TestExplicitSolarKeyBeatsTheFamilyDefault(t *testing.T) {
	saved := defaultContextLengths
	defaultContextLengths = append(append([]contextEntry{}, saved...), contextEntry{"solar-foo", 300_000})
	t.Cleanup(func() { defaultContextLengths = saved })
	mustEqual(t, upstageCtx(t, "solar-foo"), 300_000, "solar-foo")
	mustEqual(t, upstageCtx(t, "solar-foo-260101"), 300_000, "solar-foo-260101")
	mustEqual(t, upstageCtx(t, "solar-foo2"), DefaultContextLengths()["solar-"], "solar-foo2")
}

func TestEmptyModelUsesFallbackContext(t *testing.T) {
	ctx, _, _ := metaTest(t)
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: ""}), DefaultFallbackContext, "empty")
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "   "}), DefaultFallbackContext, "blank")
}

func TestKnownModelFromAPI(t *testing.T) {
	ctx, _, _ := metaTest(t)
	seedOpenRouterCatalog(map[string]map[string]any{"test/model": {"context_length": 32000}})
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "test/model"}), 32000, "catalog")
}

func TestAPIMissingContextLengthKey(t *testing.T) {
	ctx, _, _ := metaTest(t)
	seedOpenRouterCatalog(map[string]map[string]any{"test/model": {"name": "Test"}})
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "test/model"}), ContextProbeTiers[0], "missing key")
}

func TestNoBaseURLSkipsCache(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	SaveContextLength(ctx, "custom/model", "http://local", 32768, "")
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "custom/model"}), ContextProbeTiers[0], "no base url")
}

func TestStaleMinimaxCache32kIsInvalidated(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	baseURL := "https://api.minimax.io/anthropic"
	SaveContextLength(ctx, "MiniMax-M2.7", baseURL, 32768, "")
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "MiniMax-M2.7", BaseURL: baseURL, Provider: "minimax"}), 204800, "minimax")
	if _, ok := GetCachedContextLength(ctx, "MiniMax-M2.7", baseURL, false); ok {
		t.Fatal("stale entry must be dropped")
	}
}

func TestOpenRouter32kUnderreportForMinimaxFallsThroughToDefault(t *testing.T) {
	ctx, _, _ := metaTest(t)
	seedOpenRouterCatalog(map[string]map[string]any{"MiniMax-M2.7": {"context_length": 32768}})
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "MiniMax-M2.7"}), 204800, "underreport")
}

func TestNonMinimax32kCacheIsStillRespected(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	SaveContextLength(ctx, "qwen3.5:27b", "http://local", 32768, "")
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "qwen3.5:27b", BaseURL: "http://local"}), 32768, "respected")
}

func TestCustomEndpointMetadataBeatsFuzzyDefault(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", "https://llm.chutes.ai/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "zai-org/GLM-5-TEE", "context_length": 65536}}})
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "zai-org/GLM-5-TEE", BaseURL: "https://llm.chutes.ai/v1", APIKey: "test-key"}), 65536, "endpoint")
}

func TestCustomEndpointWithoutMetadataFallsBackToCatalog(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", "https://llm.chutes.ai/v1/models", 200, map[string]any{"data": []any{}})
	ft.on("GET", "https://llm.chutes.ai/models", 200, map[string]any{"data": []any{}})
	hit, _ := longestKeyMatch(defaultContextLengths, "zai-org/glm-5-tee")
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "zai-org/GLM-5-TEE", BaseURL: "https://llm.chutes.ai/v1", APIKey: "test-key"}), hit.ctx, "catalog")
}

func TestLocalOllamaPrefersNumCtxOverGGUF(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", "http://127.0.0.1:11434/api/tags", 200, map[string]any{"models": []any{}})
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, map[string]any{"parameters": "num_ctx 32768\n", "model_info": map[string]any{"llama.context_length": 131072}})
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "my-model", BaseURL: "http://localhost:11434"}), 32768, "num_ctx first")
}

func codexProxyEntry() []map[string]any {
	return []map[string]any{{"name": "codex-proxy", "base_url": "http://127.0.0.1:8317/v1", "api_mode": "codex_responses"}}
}

func TestCodexRouteBehindProxyResolvesCodexOAuthWindow(t *testing.T) {
	for _, tc := range []struct {
		provider string
		custom   []map[string]any
	}{{"custom:codex-proxy", codexProxyEntry()}, {"openai-codex", nil}} {
		ctx, ft, dir := metaTest(t)
		writeContextCacheFile(t, dir, map[string]any{"gpt-6-astra@http://127.0.0.1:8317/v1": 1_050_000})
		ft.on("GET", "http://127.0.0.1:8317/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "gpt-6-astra"}}})
		got := GetModelContextLength(ctx, ContextLengthRequest{Model: "gpt-6-astra", BaseURL: "http://127.0.0.1:8317/v1", APIKey: "proxy-key", Provider: tc.provider, CustomProviders: tc.custom})
		mustEqual(t, got, 272_000, tc.provider)
	}
}

func TestCodexProxyRouteCapsAnOptedInVariantByItsOwnCatalogMax(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	req := func(model string) ContextLengthRequest {
		return ContextLengthRequest{Model: model, BaseURL: "http://127.0.0.1:8317/v1", APIKey: "proxy-key", Provider: "custom:codex-proxy", CustomProviders: codexProxyEntry()}
	}
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 200, map[string]any{"models": []any{map[string]any{"slug": "GPT-6-Sol", "context_window": 1_050_000, "max_context_window": 600_000}}}, nil), nil
	}
	mustEqual(t, GetModelContextLength(ctx, req("gpt-6-sol-900k")), 600_000, "capped variant")
	mustEqual(t, GetModelContextLength(ctx, req("gpt-6-sol")), 272_000, "base slug")
	if ft.count() == 0 {
		t.Fatal("the route catalog must be asked")
	}
	for _, u := range ft.urls() {
		if !strings.HasPrefix(u, "http://127.0.0.1:8317/v1/") {
			t.Fatalf("probe left the route: %s", u)
		}
	}
	memoLock.Lock()
	codexOAuthContextCache = map[string]codexContextCacheEntry{}
	codexOAuthMaxContextCache = map[string]map[string]int{}
	memoLock.Unlock()
	ft.reset()
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 200, map[string]any{"data": []any{map[string]any{"id": "gpt-6-sol"}}}, nil), nil
	}
	for i := 0; i < 3; i++ {
		mustEqual(t, GetModelContextLength(ctx, req("gpt-6-sol-900k")), 900_000, "uncapped")
	}
	if probes := ft.count(); probes == 0 || probes > 2 {
		t.Fatalf("one probe expected, got %d", probes)
	}
}

func TestCodexProxyRouteExplicitContextLengthOverrideStillWins(t *testing.T) {
	ctx, _, _ := metaTest(t)
	custom := []map[string]any{{"name": "codex-proxy", "base_url": "http://127.0.0.1:8317/v1", "api_mode": "codex_responses", "models": map[string]any{"gpt-6-astra": map[string]any{"context_length": 321_000}}}}
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "gpt-6-astra", BaseURL: "http://127.0.0.1:8317/v1", Provider: "custom:codex-proxy", CustomProviders: custom}), 321_000, "override")
}

func TestStripProviderPrefixKnownProviderPrefixIsStripped(t *testing.T) {
	metaTest(t)
	cases := map[string]string{
		"local:my-model": "my-model", "openrouter:anthropic/claude-sonnet-4": "anthropic/claude-sonnet-4",
		"anthropic:claude-sonnet-4": "claude-sonnet-4", "stepfun:step-3.5-flash": "step-3.5-flash",
		"http://example.com": "http://example.com", "https://example.com": "https://example.com",
		"fireworks:accounts/fireworks/models/foo": "accounts/fireworks/models/foo",
		"not-a-provider:org/model":                "not-a-provider:org/model", "qwen3.5:27b": "qwen3.5:27b",
	}
	for in, want := range cases {
		mustEqual(t, stripProviderPrefix(in), want, in)
	}
}

func TestOllamaModelTagNotMangledInContextLookup(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", "http://127.0.0.1:11434/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "qwen3.5:27b", "context_length": 32768}}})
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "qwen3.5:27b", BaseURL: "http://localhost:11434/v1"}), 32768, "tag kept")
}

func TestFetchModelMetadataNetworkSuccessWritesDiskCache(t *testing.T) {
	ctx, ft, dir := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{map[string]any{"id": "live/model", "context_length": 67890, "name": "Live"}}})
	FetchModelMetadata(ctx, true)
	raw, err := os.ReadFile(filepath.Join(dir, "cache", "openrouter_model_metadata.json"))
	if err != nil || !strings.Contains(string(raw), "live/model") {
		t.Fatalf("disk cache missing: %v %s", err, raw)
	}
}

func TestFetchModelMetadataNetworkFailureFallsBackToStaleDiskCache(t *testing.T) {
	ctx, _, dir := metaTest(t)
	path := filepath.Join(dir, "cache", "openrouter_model_metadata.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`{"stale/model":{"context_length":50000,"name":"Stale","pricing":{}}}`), 0o600)
	old := time.Now().Add(-modelCacheTTL - time.Minute)
	os.Chtimes(path, old, old)
	got := FetchModelMetadata(ctx, true)
	mustEqual(t, intOrDefault(got["stale/model"]["context_length"], 0), 50000, "stale disk")
}

func TestFetchModelMetadataCachesResult(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{map[string]any{"id": "test/model", "context_length": 99999, "name": "Test"}}})
	if _, ok := FetchModelMetadata(ctx, true)["test/model"]; !ok {
		t.Fatal("first fetch")
	}
	mustEqual(t, ft.count(), 1, "calls")
	if _, ok := FetchModelMetadata(ctx, false)["test/model"]; !ok {
		t.Fatal("second fetch")
	}
	mustEqual(t, ft.count(), 1, "cached")
}

func TestFetchModelMetadataCanonicalSlugAliasing(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{map[string]any{"id": "anthropic/claude-3.5-sonnet:beta", "canonical_slug": "anthropic/claude-3.5-sonnet", "context_length": 200000, "name": "Claude 3.5 Sonnet"}}})
	got := FetchModelMetadata(ctx, true)
	if _, ok := got["anthropic/claude-3.5-sonnet:beta"]; !ok {
		t.Fatal("original id")
	}
	mustEqual(t, intOrDefault(got["anthropic/claude-3.5-sonnet"]["context_length"], 0), 200000, "canonical")
}

func TestContextProbeTiersTiersDescending(t *testing.T) {
	for i := 0; i+1 < len(ContextProbeTiers); i++ {
		if ContextProbeTiers[i] <= ContextProbeTiers[i+1] {
			t.Fatal("tiers must descend")
		}
	}
	next, ok := GetNextProbeTier(ContextProbeTiers[0])
	if !ok || next != ContextProbeTiers[1] {
		t.Fatal("next tier from the top")
	}
	if _, ok := GetNextProbeTier(ContextProbeTiers[len(ContextProbeTiers)-1]); ok {
		t.Fatal("no tier below the minimum")
	}
}

func TestParseContextLimitFromErrorVllmDelimiterVariants(t *testing.T) {
	cases := map[string]int{
		"max_model_len 32768": 32768, "max_model_len: 32768": 32768, "max_model_len=32768": 32768, "max_model_len (32768)": 32768, "max_model_len is 32768": 32768,
		"maximum model length 131072": 131072, "maximum model length is 131072": 131072, "maximum model length: 131072": 131072,
		"Unable to submit request because the input token count is 32825 but model only supports up to 32768. Reduce the input token count and try again.": 32768,
		"input token count is 140000 but model only supports up to 131072":                                                                                 131072,
		"model supports up to 65536 tokens":                   65536,
		"This model's maximum context length is 32768 tokens": 32768,
	}
	for msg, want := range cases {
		got, ok := ParseContextLimitFromError(msg)
		if !ok || got != want {
			t.Fatalf("%q: got %d %v want %d", msg, got, ok, want)
		}
	}
	msg := "Unable to submit request because the input token count is 32825 but model only supports up to 32768."
	if got, ok := GetContextLengthFromProviderError(msg, 131072); !ok || got != 32768 {
		t.Fatal("recalibrate downward")
	}
	if _, ok := GetContextLengthFromProviderError(msg, 32768); ok {
		t.Fatal("no recalibration at the same window")
	}
	capMsg := "max_tokens cannot exceed the configured model output limit of 16384"
	if _, ok := ParseContextLimitFromError(capMsg); ok {
		t.Fatal("output cap is not a context limit")
	}
	if got, ok := ParseAvailableOutputTokensFromError(capMsg); !ok || got != 16384 {
		t.Fatal("output cap budget")
	}
	if !IsOutputCapError(capMsg) {
		t.Fatal("output cap error")
	}
}

func TestParseAvailableOutputTokensWordings(t *testing.T) {
	cases := map[string]int{
		"max_tokens: 100000 > 64000, which is the maximum allowed number of output tokens":     64000,
		"Range of max_tokens should be [1, 65536]":                                             65536,
		"max_tokens exceeds model's maximum output tokens (65536)":                             65536,
		"max_tokens is too large: 65536. This model supports at most 32768 completion tokens.": 32768,
		"max_completion_tokens is limited to 16384 for glm-5.2":                                16384,
		"This model's maximum context length is 200000 tokens. However, you requested 210000 tokens (150000 of text input, 10000 of tool input, 50000 in the output)": 40000,
		"This model's maximum context length is 65536 tokens. However, you requested 102401 tokens (36865 in the messages, 65536 in the completion).":                 28671,
		"The model's maximum context length is 8192 tokens, but the prompt contains 3000 characters and requested 8000 output tokens":                                 7192,
		"maximum context length of 131072 tokens. You requested a total of 132528 tokens: 66992 tokens from the input messages and 65536 tokens for the completion":   64080,
		"This model's maximum context length is 32768 tokens. However, your prompt contains 30000 input tokens and you requested 5000 output tokens":                  2768,
		"This model's maximum context length is 32768 tokens. However, your prompt contains at least 28769 input tokens and you requested 4000 output tokens":         2000,
	}
	for msg, want := range cases {
		got, ok := ParseAvailableOutputTokensFromError(msg)
		if !ok || got != want {
			t.Fatalf("%q: got %d %v want %d", msg, got, ok, want)
		}
	}
	if _, ok := ParseAvailableOutputTokensFromError("prompt is too long"); ok {
		t.Fatal("input overflow has no output budget")
	}
	if IsOutputCapError("maximum context length is 32768 tokens, your prompt is too long, requested max_tokens must be reduced") {
		t.Fatal("an oversized input is a genuine overflow")
	}
	if !IsOutputCapError("This model's maximum context length is 65536 tokens. However, you requested 102401 tokens (36865 in the messages, 65536 in the completion). Please reduce the length.") {
		t.Fatal("the measured prompt decides")
	}
	if IsOutputCapError("maximum context length of 131072 tokens. 140000 tokens from the input messages and 65536 tokens for the completion. max_tokens") {
		t.Fatal("sglang input above the window is an overflow")
	}
}

func TestNonPositiveLengthsNeverPersisted(t *testing.T) {
	ctx, _, _ := metaTest(t)
	SaveContextLength(ctx, "test/model", "http://x", 0, "")
	SaveContextLength(ctx, "test/model", "http://x", -1, "")
	if _, ok := GetCachedContextLength(ctx, "test/model", "http://x", false); ok {
		t.Fatal("nothing persisted")
	}
}

func TestNonPositiveCachedEntryDroppedAndReresolved(t *testing.T) {
	ctx, ft, dir := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	os.WriteFile(filepath.Join(dir, "context_length_cache.yaml"), []byte("context_lengths:\n  test/model@http://x: 0\n"), 0o600)
	if got, ok := GetCachedContextLength(ctx, "test/model", "http://x", false); !ok || got != 0 {
		t.Fatal("poison entry readable")
	}
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: "test/model", BaseURL: "http://x"}); got <= 0 {
		t.Fatal("re-resolved")
	}
	if got, ok := GetCachedContextLength(ctx, "test/model", "http://x", false); ok && got == 0 {
		t.Fatal("poison entry must be dropped")
	}
}

func TestNullContextLengthsKeyReturnsEmpty(t *testing.T) {
	ctx, _, dir := metaTest(t)
	os.WriteFile(filepath.Join(dir, "context_length_cache.yaml"), []byte("context_lengths:\n"), 0o600)
	if _, ok := GetCachedContextLength(ctx, "test/model", "http://x", false); ok {
		t.Fatal("null section is empty")
	}
	SaveContextLength(ctx, "test/model", "http://x", 32768, "")
	if got, ok := GetCachedContextLength(ctx, "test/model", "http://x", false); !ok || got != 32768 {
		t.Fatal("save repairs the file")
	}
}

func TestIdempotentSave(t *testing.T) {
	ctx, _, dir := metaTest(t)
	SaveContextLength(ctx, "model", "http://x", 32768, "")
	SaveContextLength(ctx, "model", "http://x", 32768, "")
	mustEqual(t, len(readContextCacheFile(t, dir)), 1, "entries")
}

func TestCachedValueTakesPriority(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	SaveContextLength(ctx, "unknown/model", "http://local", 65536, "")
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "unknown/model", BaseURL: "http://local"}), 65536, "cached")
}

func TestWriteFailureLeavesExistingCacheIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ctx, _, dir := metaTest(t)
	SaveContextLength(ctx, "model-a", "http://a", 64000, "")
	path := filepath.Join(dir, "context_length_cache.yaml")
	original, _ := os.ReadFile(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	SaveContextLength(ctx, "model-b", "http://b", 128000, "")
	after, _ := os.ReadFile(path)
	if string(after) != string(original) {
		t.Fatal("original file must survive a failed write")
	}
	os.Chmod(dir, 0o700)
	if got, ok := GetCachedContextLength(ctx, "model-a", "http://a", false); !ok || got != 64000 {
		t.Fatal("model-a survives")
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(entries) != 0 {
		t.Fatalf("stray temp files: %v", entries)
	}
}

func TestStaleGrok43DroppedAndReresolvesTo1M(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	base := "https://api.x.ai/v1"
	SaveContextLength(ctx, "grok-4.3", base, 256_000, "")
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "grok-4.3", BaseURL: base, Provider: "xai"}), 1_000_000, "grok-4.3")
}

func TestGrok4NotClobbered(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	base := "https://api.x.ai/v1"
	for _, slug := range []string{"grok-4", "grok-4-0709"} {
		SaveContextLength(ctx, slug, base, 256_000, "")
		mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: slug, BaseURL: base, Provider: "xai"}), 256_000, slug)
	}
}

func TestGenericPreCatalogStaleGuard(t *testing.T) {
	if !stalePreCatalogCacheEntry("qwen3.6-plus", 131_072) || !stalePreCatalogCacheEntry("alibaba/qwen3.6-plus", 131_072) {
		t.Fatal("qwen3.6-plus leftover")
	}
	if stalePreCatalogCacheEntry("qwen3.6-plus", 1_048_576) || stalePreCatalogCacheEntry("qwen3.6-plus", 262_144) {
		t.Fatal("genuine values survive")
	}
	if stalePreCatalogCacheEntry("qwen3-coder", 131_072) {
		t.Fatal("sibling slugs untouched")
	}
	if stalePreCatalogCacheEntry("totally-unknown-model", 4096) || stalePreCatalogCacheEntry("minimax", 204_800) {
		t.Fatal("unknown models never dropped")
	}
}

func TestFallbackWarningWarningEmittedOnFallback(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	buf := captureMetadataLog(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	for i := 0; i < 3; i++ {
		mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "totally-unknown-model-xyz"}), DefaultFallbackContext, "fallback")
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "totally-unknown-model-xyz") || !strings.Contains(out, "model.context_length") {
		t.Fatalf("warning missing: %s", out)
	}
	mustEqual(t, strings.Count(out, "falling back"), 1, "deduped")
}

func TestFallbackWarningWarningEmittedOnCustomEndpointFallback(t *testing.T) {
	ctx, _, _ := metaTest(t)
	buf := captureMetadataLog(t)
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "totally-unknown-model-xyz", BaseURL: "http://192.168.1.50:8080/v1"}), DefaultFallbackContext, "custom fallback")
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "totally-unknown-model-xyz") || !strings.Contains(out, "model.context_length") {
		t.Fatalf("warning missing: %s", out)
	}
}

func TestNoWarningWhenCached(t *testing.T) {
	ctx, _, _ := metaTest(t)
	buf := captureMetadataLog(t)
	SaveContextLength(ctx, "some-model", "http://127.0.0.1:1/v1", 32_000, "")
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "some-model", BaseURL: "http://127.0.0.1:1/v1"}), 32_000, "cached")
	if strings.Contains(buf.String(), "falling back") {
		t.Fatal("no fallback warning when cached")
	}
}

func TestOpenRouterRoutingVariantContextLengthVariantMatchesBaseButRealSKUKeepsOwnWindow(t *testing.T) {
	ctx, _, _ := metaTest(t)
	seedOpenRouterCatalog(map[string]map[string]any{
		"x-ai/grok-4.6": {"context_length": 2_000_000}, "thinkingmachines/inkling": {"context_length": 1_000_000}, "thinkingmachines/inkling:free": {"context_length": 64_000},
	})
	for _, suffix := range []string{"nitro", "floor", "exacto", "online"} {
		mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "x-ai/grok-4.6", Provider: "openrouter"}), 2_000_000, "base")
		mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "x-ai/grok-4.6:" + suffix, Provider: "openrouter"}), 2_000_000, suffix)
		mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "thinkingmachines/inkling:free", Provider: "openrouter"}), 64_000, "free sku")
	}
}

func TestEndpointPricingAlreadyPerMillionIsNotInflated(t *testing.T) {
	entry := endpointModelEntry(map[string]any{"id": "minimax-m3", "pricing": map[string]any{"currency": "USD", "prompt": 0.6, "completion": 1.2, "cache_read": 0.12}}, "minimax-m3", 0, false)
	pricing := entry["pricing"].(map[string]any)
	mustEqual(t, pricing["prompt"].(string), "6e-07", "prompt")
	mustEqual(t, pricing["cache_read"].(string), "1.2e-07", "cache read")
	entry = endpointModelEntry(map[string]any{"id": "glm-x", "pricing": map[string]any{"currency": "CNY", "unit": "per_1m_tokens", "prompt": 1, "completion": 2}}, "glm-x", 0, false)
	mustEqual(t, entry["pricing"].(map[string]any)["completion"].(string), "2e-06", "explicit unit")
	entry = endpointModelEntry(map[string]any{"id": "crof-a", "cost": map[string]any{"input": 0.04, "output": 0.15}}, "crof-a", 0, false)
	mustEqual(t, entry["pricing"].(map[string]any)["prompt"].(string), "4e-08", "nested cost")
	entry = endpointModelEntry(map[string]any{"id": "m", "pricing": map[string]any{"prompt": "0.0000006", "completion": "0.0000012", "request": "0.005"}}, "m", 0, false)
	pricing = entry["pricing"].(map[string]any)
	mustEqual(t, pricing["prompt"].(string), "0.0000006", "per token passes through")
	mustEqual(t, pricing["request"].(string), "0.005", "request fee")
}

func TestLongestDefaultContextMatchReadsTheDefaultTable(t *testing.T) {
	if n, ok := LongestDefaultContextMatch("anthropic/claude-opus-4-8-20260101"); !ok || n != 1000000 {
		t.Fatalf("longest default match = %d %v", n, ok)
	}
	if _, ok := LongestDefaultContextMatch("no-such-model-family-xyz"); ok {
		t.Fatal("unknown model matched the default table")
	}
}

func TestStripProviderPrefixExportMatchesUnexported(t *testing.T) {
	for _, m := range []string{"openrouter:anthropic/claude-sonnet-4", "qwen3:32b", "https://host:8080/m", "plain"} {
		if got, want := StripProviderPrefix(m), stripProviderPrefix(m); got != want {
			t.Fatalf("strip %q = %q, want %q", m, got, want)
		}
	}
}
