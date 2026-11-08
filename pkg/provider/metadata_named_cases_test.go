package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestStripProviderPrefixHttpUrlsPreserved(t *testing.T) {
	metaTest(t)
	for _, u := range []string{"http://example.com", "https://example.com"} {
		mustEqual(t, stripProviderPrefix(u), u, u)
	}
}

func TestStripProviderPrefixRegisteredProfileNameAndAliasAreStripped(t *testing.T) {
	metaTest(t)
	registerTestProfile(t, Profile{Name: "fake-provider", Aliases: []string{"fake-alias"}})
	mustEqual(t, stripProviderPrefix("fake-provider:org/model"), "org/model", "profile name")
	mustEqual(t, stripProviderPrefix("fake-alias:org/model"), "org/model", "profile alias")
}

func TestStripProviderPrefixBundledPluginProviderPrefixIsStripped(t *testing.T) {
	metaTest(t)
	mustEqual(t, stripProviderPrefix("fireworks:accounts/fireworks/models/foo"), "accounts/fireworks/models/foo", "fireworks")
}

func TestStripProviderPrefixUnknownProviderPrefixIsUnchanged(t *testing.T) {
	metaTest(t)
	mustEqual(t, stripProviderPrefix("not-a-provider:org/model"), "not-a-provider:org/model", "unknown")
}

func TestStripProviderPrefixOllamaModelTagIsUnchanged(t *testing.T) {
	metaTest(t)
	mustEqual(t, stripProviderPrefix("qwen3.5:27b"), "qwen3.5:27b", "ollama tag")
}

func TestParseContextLimitFromErrorGoogleSupportsUpToVariants(t *testing.T) {
	cases := map[string]int{
		"Unable to submit request because the input token count is 32825 but model only supports up to 32768. Reduce the input token count and try again.": 32768,
		"input token count is 140000 but model only supports up to 131072":                                                                                 131072,
		"model supports up to 65536 tokens": 65536,
	}
	for msg, want := range cases {
		if got, ok := ParseContextLimitFromError(msg); !ok || got != want {
			t.Fatalf("%q: got %d %v want %d", msg, got, ok, want)
		}
	}
}

func TestParseContextLimitFromErrorGoogleSupportsUpToRecalibratesWindow(t *testing.T) {
	msg := "Unable to submit request because the input token count is 32825 but model only supports up to 32768."
	if got, ok := GetContextLengthFromProviderError(msg, 131072); !ok || got != 32768 {
		t.Fatal("recalibrate downward")
	}
	if _, ok := GetContextLengthFromProviderError(msg, 32768); ok {
		t.Fatal("no recalibration at the same window")
	}
}

func TestParseContextLimitFromErrorOutputCapMessageIsNotAContextLimit(t *testing.T) {
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
	if got, ok := ParseContextLimitFromError("This model's maximum context length is 32768 tokens"); !ok || got != 32768 {
		t.Fatal("genuine context messages still parse")
	}
}

func TestGenericPreCatalogStaleGuardUnknownModelsNeverDropped(t *testing.T) {
	if stalePreCatalogCacheEntry("totally-unknown-model", 4096) || stalePreCatalogCacheEntry("minimax", 204_800) {
		t.Fatal("unknown models never dropped")
	}
}

func TestFallbackWarningWarningFiresOncePerModel(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	buf := captureMetadataLog(t)
	ft.on("GET", OpenRouterModelsURL, 200, map[string]any{"data": []any{}})
	for range 3 {
		GetModelContextLength(ctx, ContextLengthRequest{Model: "totally-unknown-model-xyz"})
	}
	mustEqual(t, strings.Count(buf.String(), "falling back"), 1, "one fallback warning per model")
}

func TestEndpointPricingPerTokenQuotesPassThroughUnchanged(t *testing.T) {
	entry := endpointModelEntry(map[string]any{"id": "m", "pricing": map[string]any{"prompt": "0.0000006", "completion": "0.0000012", "request": "0.005"}}, "m", 0, false)
	pricing, ok := pricingEntryFromMetadata(map[string]map[string]any{"m": entry}, "m", "x", "openai-compatible-models-api")
	if !ok {
		t.Fatal("no pricing entry")
	}
	input, _ := pricing.InputCostPerMillion.Float64()
	output, _ := pricing.OutputCostPerMillion.Float64()
	request, _ := pricing.RequestCost.Float64()
	if input < 0.59 || input > 0.61 || output < 1.19 || output > 1.21 || request < 0.0049 || request > 0.0051 {
		t.Fatalf("per token quotes changed: %v %v %v", input, output, request)
	}
}

func TestGetNextProbeTierFrom256k(t *testing.T) {
	if next, ok := GetNextProbeTier(ContextProbeTiers[0]); !ok || next != ContextProbeTiers[1] {
		t.Fatal("next tier from the top")
	}
}

func TestGetNextProbeTierFrom8kReturnsNone(t *testing.T) {
	if _, ok := GetNextProbeTier(ContextProbeTiers[len(ContextProbeTiers)-1]); ok {
		t.Fatal("no tier below the minimum")
	}
}

func TestFetchEndpointModelMetadataAuthFailureEmptyResultIsCached(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return jsonResponse(nil, 401, map[string]any{}, nil), nil }
	first := FetchEndpointModelMetadata(ctx, "https://custom.example/v1", "", false)
	second := FetchEndpointModelMetadata(ctx, "https://custom.example/v1", "", false)
	if len(first) != 0 || len(second) != 0 || ft.count() != 1 {
		t.Fatalf("first %v second %v after %d requests", first, second, ft.count())
	}
}

func TestCodexOAuthContextLengthFallbackTableBaseSlugStays272k(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return jsonResponse(nil, 401, map[string]any{}, nil), nil }
	mustEqual(t, GetModelContextLength(ctx, codexContextRequest("gpt-5.6-sol")), 272_000, "base slug")
}

func TestBedrockContextCachePersistenceFailedProbeIsMemoisedUntilTheTtlLapses(t *testing.T) {
	bedrockIsolatedEnv(t)
	calls := bedrockStubProbe(t, 0, 1_000_000)
	ctx := context.Background()
	model := "amazon.future-model-v1:0"
	for range 2 {
		if got := GetModelContextLength(ctx, ContextLengthRequest{Model: model, Provider: "bedrock"}); got != BedrockDefaultContextLength {
			t.Fatalf("fallback %d", got)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("%d probes inside the TTL", len(*calls))
	}
	if _, err := os.Stat(contextCachePath(ctx)); !os.IsNotExist(err) {
		t.Fatal("the failure memo must stay in memory")
	}
	bedrockProbeFailureMu.Lock()
	for key := range bedrockProbeFailureCache {
		bedrockProbeFailureCache[key] = monotonicNow().Add(-bedrockProbeFailureTTL - time.Second)
	}
	bedrockProbeFailureMu.Unlock()
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: model, Provider: "bedrock"}); got != 1_000_000 {
		t.Fatalf("after the TTL resolved %d", got)
	}
	if got, ok := GetCachedContextLength(ctx, model, "bedrock://", false); !ok || got != 1_000_000 {
		t.Fatalf("cached %d %t", got, ok)
	}
	if len(*calls) != 2 {
		t.Fatalf("%d probes", len(*calls))
	}
}

func TestQueryLocalContextLengthVllmVllmContextLengthKey(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:8000/v1", "vllm")
	ft.on("GET", "http://127.0.0.1:8000/v1/models/some-model", 200, map[string]any{"id": "some-model", "context_length": 32768})
	mustEqual(t, queryLocalContextLength(ctx, "some-model", "http://localhost:8000/v1", ""), 32768, "context_length key")
}

func TestQueryLocalContextLengthVllmDetailBranchReadsContextWindowNotOutputCap(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:8000/v1", "vllm")
	ft.on("GET", "http://127.0.0.1:8000/v1/models/some-model", 200, map[string]any{"type": "model", "id": "some-model", "max_input_tokens": 1000000, "max_tokens": 128000})
	mustEqual(t, queryLocalContextLength(ctx, "some-model", "http://localhost:8000/v1", ""), 1000000, "window not output cap")
}

func TestModelsListLlamacppPrefersRuntimeNCtxOverTrain(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:8080", "")
	ft.on("GET", "http://127.0.0.1:8080/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "/app/models/m.gguf", "meta": map[string]any{"n_ctx": 256000, "n_ctx_train": 262144}}}})
	mustEqual(t, queryLocalContextLength(ctx, "m", "http://localhost:8080", ""), 256000, "runtime n_ctx")
}

func TestContextLengthFromModelPayloadFallsBackToMaxTokensWhenNoInputWindowField(t *testing.T) {
	if n, ok := contextLengthFromModelPayload(map[string]any{"id": "odd-server", "max_tokens": 65536}); !ok || n != 65536 {
		t.Fatal("max_tokens last resort")
	}
}

func TestContextLengthFromModelPayloadReturnsNoneForEmptyPayload(t *testing.T) {
	if _, ok := contextLengthFromModelPayload(map[string]any{}); ok {
		t.Fatal("empty payload")
	}
	if _, ok := contextLengthFromModelPayload(nil); ok {
		t.Fatal("nil payload")
	}
}

func TestQueryLocalContextLengthAnthropicProxyModelDetailPrefersMaxInputTokens(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://127.0.0.1:47821/v1", "vllm")
	ft.on("GET", "http://127.0.0.1:47821/v1/models/claude-fable-5", 200, map[string]any{"type": "model", "id": "claude-fable-5", "max_input_tokens": 1_000_000, "max_tokens": 128_000})
	mustEqual(t, queryLocalContextLength(ctx, "claude-fable-5", "http://127.0.0.1:47821/v1", ""), 1_000_000, "detail max_input_tokens")
}

func TestDetectLocalServerTypeAuthNativeApiBaseUrlIsNotDoubled(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return jsonResponse(nil, 200, map[string]any{}, nil), nil }
	mustEqual(t, DetectLocalServerType(ctx, "http://localhost:1234/api/v1", ""), "lm-studio", "native base")
	mustEqual(t, ft.urls()[0], "http://127.0.0.1:1234/api/v1/models", "not doubled")
}

func TestDetectLocalServerTypeLocalhostIPv4NonLocalhostUrlsUnchanged(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return jsonResponse(nil, 404, map[string]any{}, nil), nil }
	DetectLocalServerType(ctx, "http://192.168.1.100:8080", "")
	for _, u := range ft.urls() {
		if !strings.Contains(u, "192.168.1.100") {
			t.Fatalf("non localhost url changed: %s", u)
		}
	}
}

func TestFetchEndpointModelMetadataLmStudioNativeApiBaseUrlIsNotDoubled(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:1234/api/v1", "lm-studio")
	ft.on("GET", "http://localhost:1234/api/v1/models", 200, map[string]any{"models": []any{map[string]any{"key": "publisher/model-a", "id": "publisher/model-a", "loaded_instances": []any{map[string]any{"config": map[string]any{"context_length": 65536}}}}}})
	got := FetchEndpointModelMetadata(ctx, "http://localhost:1234/api/v1", "", true)
	mustEqual(t, ft.calls[0].URL, "http://localhost:1234/api/v1/models", "not doubled")
	mustEqual(t, intOrDefault(got["publisher/model-a"]["context_length"], 0), 65536, "entry")
}

func TestKnownServerTypeProbesSingleModel(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://vllm-server:8000/v1", "vllm")
	ft.on("GET", "http://vllm-server:8000/v1/models/my-model", 200, map[string]any{"max_model_len": 65536})
	mustEqual(t, queryLocalContextLength(ctx, "my-model", "http://vllm-server:8000/v1", ""), 65536, "detail")
	probed := false
	for _, u := range ft.urls() {
		if strings.HasSuffix(u, "/v1/models/my-model") {
			probed = true
		}
	}
	if !probed {
		t.Fatalf("a known server type must probe the single model: %v", ft.urls())
	}
}

func TestLocalContextProbeTTLCacheDifferentKeyStillProbes(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434/v1", "ollama")
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, map[string]any{"model_info": map[string]any{"llama.context_length": 32768}})
	queryLocalContextLength(ctx, "m1", "http://localhost:11434/v1", "")
	queryLocalContextLength(ctx, "m2", "http://localhost:11434/v1", "")
	mustEqual(t, ft.count(), 2, "a different key probes")
}

func TestLocalContextProbeTTLCacheNoneResultNotCached(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434/v1", "")
	ft.handle = func(*http.Request) (*http.Response, error) { return jsonResponse(nil, 404, map[string]any{}, nil), nil }
	mustEqual(t, queryLocalContextLength(ctx, "m", "http://localhost:11434/v1", ""), 0, "miss")
	first := ft.count()
	mustEqual(t, queryLocalContextLength(ctx, "m", "http://localhost:11434/v1", ""), 0, "retry")
	if ft.count() <= first {
		t.Fatal("a failed probe must not be memoized")
	}
}

func TestPoisonedCacheHealsEndToEndFromProbePayload(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	model, base := "deepseek-v4-flash", "http://127.0.0.1:8080/v1"
	seedServerType(base, "")
	ft.on("GET", "http://127.0.0.1:8080/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": model, "context_size": 1048576, "max_tokens": 393216}}})
	SaveContextLength(ctx, model, base, 393216, "")
	mustEqual(t, reconcileLocalCachedContextLength(ctx, model, base, 393216, ""), 1048576, "healed")
	if got, ok := GetCachedContextLength(ctx, model, base, false); !ok || got != 1048576 {
		t.Fatalf("cache row %d %t", got, ok)
	}
}

// MoA preset config naming the aggregator the virtual provider mirrors
func writeMoaConfig(t *testing.T, dir string, aggregator map[string]any) {
	t.Helper()
	payload := map[string]any{"moa": map[string]any{"default_preset": "p", "presets": map[string]any{"p": map[string]any{
		"enabled": true, "reference_models": []any{map[string]any{"provider": "openrouter", "model": "openai/gpt-5.5"}}, "aggregator": aggregator}}}}
	raw, err := yaml.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMoAContextLengthMoaResolvesFromAggregator(t *testing.T) {
	ctx, _, dir := metaTest(t)
	writeMoaConfig(t, dir, map[string]any{"provider": "openrouter", "model": "anthropic/claude-opus-4.8"})
	aggregator := GetModelContextLength(ctx, ContextLengthRequest{Model: "anthropic/claude-opus-4.8", BaseURL: "https://openrouter.ai/api/v1", Provider: "openrouter"})
	moa := GetModelContextLength(ctx, ContextLengthRequest{Model: "p", BaseURL: "http://127.0.0.1/v1", Provider: "moa"})
	if moa != aggregator || moa == 0 {
		t.Fatalf("moa %d aggregator %d", moa, aggregator)
	}
}

func TestDefaultContextLengthsXaiOauthGrokBuildUsesXaiModelsDevContext(t *testing.T) {
	ctx, _, dir := metaTest(t)
	ResetModelsDevCache()
	t.Cleanup(ResetModelsDevCache)
	registry := map[string]any{"xai": map[string]any{"id": "xai", "models": map[string]any{"grok-build-0.1": map[string]any{"limit": map[string]any{"context": 256000, "output": 64000}}}}}
	raw, _ := json.Marshal(registry)
	if err := os.MkdirAll(home.CacheDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.CacheDir(dir), "models_dev_cache.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := downloadModelsDev
	downloadModelsDev = func(context.Context, string, map[string]string) (int, any, string, error) {
		return 0, nil, "", errors.New("offline")
	}
	t.Cleanup(func() { downloadModelsDev = prev })
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: "grok-build-0.1", Provider: "xai-oauth", BaseURL: "https://api.x.ai/v1", APIKey: "oauth-token"}); got != 256000 {
		t.Fatalf("resolved %d", got)
	}
}
