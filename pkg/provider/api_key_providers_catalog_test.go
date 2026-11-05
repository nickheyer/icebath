package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Catalog opener seam answering every pricing GET with payload decoded the way the opener parses JSON
func pricingPayloadStub(t *testing.T, payload any, seen func(url string, headers map[string]string, timeout time.Duration)) {
	t.Helper()
	prev := pricingGetJSON
	pricingGetJSON = func(url string, headers map[string]string, timeout time.Duration) map[string]any {
		if seen != nil {
			seen(url, headers, timeout)
		}
		if payload == nil {
			return nil
		}
		raw, _ := json.Marshal(payload)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	t.Cleanup(func() { pricingGetJSON = prev; ResetPricingCache() })
	ResetPricingCache()
}

// Models catalog transport answering every request with payload
func deepinfraCatalogStub(payload any, seen func(req *http.Request)) {
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		if seen != nil {
			seen(req)
		}
		return modelsJSONResponse(req, 200, payload), nil
	})
}

func TestNovitaPricingCache(t *testing.T) {
	apiKeyProvidersEnv(t)
	t.Setenv("NOVITA_API_KEY", "sk-test-key")
	t.Setenv("NOVITA_BASE_URL", "https://api.novita.ai/openai/v1")
	calls := 0
	pricingPayloadStub(t, map[string]any{"data": []any{map[string]any{"id": "x/y", "input_token_price_per_m": 1000,
		"output_token_price_per_m": 2000}}}, func(string, map[string]string, time.Duration) { calls++ })
	first := fetchNovitaPricing(0, false)
	if _, ok := first["x/y"]; !ok || calls != 1 {
		t.Fatalf("first fetch %v calls=%d", first, calls)
	}
	second := fetchNovitaPricing(0, false)
	if !mapsEqualJSON(second, first) || calls != 1 {
		t.Fatalf("cached fetch %v calls=%d", second, calls)
	}
	fetchNovitaPricing(0, true)
	if calls != 2 {
		t.Fatalf("force refresh calls=%d", calls)
	}
}

// JSON equality of two decoded values
func mapsEqualJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestMinimaxOAuthAuxModelRegistered(t *testing.T) {
	p, ok := ProfileFor("minimax-oauth")
	if !ok {
		t.Fatal("minimax-oauth provider profile must be registered")
	}
	if p.DefaultAuxModel == "" {
		t.Fatal("minimax-oauth profile must advertise a non-empty default_aux_model")
	}
}

func TestFetchDeepInfraModelsReturnsFilteredModelsOnSuccess(t *testing.T) {
	ctx := modelsTestHome(t)
	t.Setenv("DEEPINFRA_API_KEY", "test-key")
	deepinfraCatalogStub(map[string]any{"data": []any{
		map[string]any{"id": "meta-llama/Llama-3-70B-Instruct", "metadata": map[string]any{}},
		map[string]any{"id": "mistralai/Mistral-Nemo-Instruct-2407", "metadata": map[string]any{}},
		map[string]any{"id": "BAAI/bge-large-en-v1.5-embed", "metadata": map[string]any{}},
		map[string]any{"id": "stabilityai/stable-diffusion-xl-base-1.0", "metadata": map[string]any{}},
	}}, nil)
	result, ok := fetchDeepinfraModels(ctx, 0, false)
	if !ok || !slices.Contains(result, "meta-llama/Llama-3-70B-Instruct") || !slices.Contains(result, "mistralai/Mistral-Nemo-Instruct-2407") {
		t.Fatalf("models %v %v", result, ok)
	}
	for _, m := range result {
		if strings.Contains(strings.ToLower(m), "embed") || strings.Contains(strings.ToLower(m), "stable-diffusion") {
			t.Fatalf("non chat model kept %q", m)
		}
	}
}

func TestDeepInfraCatalogUsesCredentialSafeOpener(t *testing.T) {
	ctx := modelsTestHome(t)
	t.Setenv("DEEPINFRA_API_KEY", "test-key")
	var auth string
	var timeout time.Duration
	deepinfraCatalogStub(map[string]any{"data": []any{}}, func(req *http.Request) {
		auth = req.Header.Get("Authorization")
		if deadline, ok := req.Context().Deadline(); ok {
			timeout = time.Until(deadline).Round(time.Second)
		}
	})
	got, ok := fetchDeepinfraCatalog(ctx, 0, true)
	if !ok || got == nil || len(got) != 0 {
		t.Fatalf("catalog %v %v", got, ok)
	}
	if auth != "Bearer test-key" || timeout != 5*time.Second {
		t.Fatalf("opener saw authorization=%q timeout=%v", auth, timeout)
	}
}

func TestDeepInfraTagFilteringBySurfaceTagAndRolloutStates(t *testing.T) {
	ctx := modelsTestHome(t)
	payload := map[string]any{"data": []any{
		map[string]any{"id": "vendor/chat-tagged", "metadata": map[string]any{"tags": []any{"chat"}}},
		map[string]any{"id": "vendor/image-tagged", "metadata": map[string]any{"tags": []any{"image-gen"}}},
		map[string]any{"id": "vendor/tts-tagged", "metadata": map[string]any{"tags": []any{"tts"}}},
		map[string]any{"id": "vendor/stt-tagged", "metadata": map[string]any{"tags": []any{"stt"}}},
		map[string]any{"id": "vendor/embed-tagged", "metadata": map[string]any{"tags": []any{"embed"}}},
		map[string]any{"id": "Qwen/Qwen3-30B", "metadata": map[string]any{"tags": []any{"reasoning", "vision"}}},
		map[string]any{"id": "openai/whisper-large", "metadata": map[string]any{"tags": []any{"reasoning"}}},
		map[string]any{"id": "some-org/whisper-finetune-chat", "metadata": map[string]any{"tags": []any{"chat"}}},
		map[string]any{"id": "stub-model", "metadata": nil},
	}}
	for _, surface := range []string{"chat", "image-gen", "tts", "stt", "embed"} {
		deepinfraCatalogStub(payload, nil)
		deepinfraCatalogMu.Lock()
		deepinfraCatalogCache = map[string][]map[string]any{}
		deepinfraCatalogMu.Unlock()
		got, ok := fetchDeepinfraModelsByTagIn(ctx, surface, 0, false)
		if !ok || got == nil {
			t.Fatalf("%s: no result", surface)
		}
		ids := map[string]bool{}
		for _, item := range got {
			ids[stringOf(item["id"])] = true
		}
		if ids["stub-model"] {
			t.Fatalf("%s: null metadata kept", surface)
		}
		if surface == "chat" {
			if !ids["vendor/chat-tagged"] || !ids["Qwen/Qwen3-30B"] || !ids["some-org/whisper-finetune-chat"] || ids["openai/whisper-large"] {
				t.Fatalf("chat ids %v", ids)
			}
			continue
		}
		for _, item := range got {
			tags, _ := asList(dictOrEmpty(item["metadata"])["tags"])
			if !slices.Contains(tags, any(surface)) {
				t.Fatalf("%s: item %v lacks the tag", surface, item)
			}
		}
	}
}

func TestDeepInfraPricingShapeAndDispatch(t *testing.T) {
	ctx := modelsTestHome(t)
	ResetPricingCache()
	t.Cleanup(ResetPricingCache)
	deepinfraCatalogStub(map[string]any{"data": []any{
		map[string]any{"id": "vendor/model-a", "metadata": map[string]any{"tags": []any{"chat", "prompt_cache"},
			"pricing": map[string]any{"input_tokens": 0.1, "output_tokens": 0.3, "cache_read_tokens": 0.02}}},
		map[string]any{"id": "vendor/model-b", "metadata": map[string]any{"tags": []any{"chat"},
			"pricing": map[string]any{"input_tokens": 1.0, "output_tokens": 5.0}}},
		map[string]any{"id": "vendor/model-image", "metadata": map[string]any{"tags": []any{"image-gen"},
			"pricing": map[string]any{"per_image_unit": 0.05}}},
	}}, nil)
	result := GetPricingForProvider(ctx, "deepinfra", "", false, false)
	if len(result) != 2 || result["vendor/model-a"] == nil || result["vendor/model-b"] == nil {
		t.Fatalf("pricing ids %v", result)
	}
	approx := func(v any, want float64) bool {
		f, ok := floatOf(v)
		return ok && f > want*(1-1e-6) && f < want*(1+1e-6)
	}
	if !approx(result["vendor/model-a"]["prompt"], 0.1/1_000_000) || !approx(result["vendor/model-a"]["completion"], 0.3/1_000_000) {
		t.Fatalf("model-a %v", result["vendor/model-a"])
	}
	if _, ok := result["vendor/model-a"]["input_cache_read"]; !ok {
		t.Fatalf("model-a lost its cache read price %v", result["vendor/model-a"])
	}
	if _, ok := result["vendor/model-b"]["input_cache_read"]; ok {
		t.Fatalf("model-b gained a cache read price %v", result["vendor/model-b"])
	}
}

func TestKilocodePricingShapeAndDispatch(t *testing.T) {
	apiKeyProvidersEnv(t)
	pricingPayloadStub(t, map[string]any{"data": []any{
		map[string]any{"id": "vendor/paid", "pricing": map[string]any{"prompt": "0.000004", "completion": "0.00002",
			"input_cache_read": "0.0000004"}},
		map[string]any{"id": "kilo-auto/free", "isFree": true, "pricing": map[string]any{"prompt": "0.000002", "completion": "0.00001"}},
		map[string]any{"id": "kilo-auto/efficient", "pricing": map[string]any{"prompt": "-1", "completion": "-1"}},
		map[string]any{"id": "vendor/no-pricing"},
		map[string]any{"id": ""},
		"not-a-dict",
	}}, nil)
	result := GetPricingForProvider(context.Background(), "kilocode", "", false, false)
	want := PricingTable{
		"vendor/paid":    {"prompt": "0.000004", "completion": "0.00002", "input_cache_read": "0.0000004"},
		"kilo-auto/free": {"prompt": "0", "completion": "0"},
	}
	if !mapsEqualJSON(result, want) {
		t.Fatalf("kilocode pricing %v", result)
	}
}

func TestKilocodePricingEndpointComesFromProfileBaseURLAndIsPublic(t *testing.T) {
	apiKeyProvidersEnv(t)
	type hit struct{ url, auth string }
	var seen []hit
	payload := map[string]any{"data": []any{map[string]any{"id": "a/b", "pricing": map[string]any{"prompt": "0.1", "completion": "0.2"}}}}
	pricingPayloadStub(t, payload, func(url string, headers map[string]string, _ time.Duration) {
		seen = append(seen, hit{url, headers["Authorization"]})
	})
	want := PricingTable{"a/b": {"prompt": "0.1", "completion": "0.2"}}
	if got := GetPricingForProvider(context.Background(), "kilocode", "", true, false); !mapsEqualJSON(got, want) {
		t.Fatalf("pricing %v", got)
	}
	if len(seen) != 1 || seen[0] != (hit{"https://api.kilo.ai/api/gateway/models", ""}) {
		t.Fatalf("requests %v", seen)
	}
	t.Setenv("KILOCODE_BASE_URL", "https://proxy.example.com/kilo")
	if got := GetPricingForProvider(context.Background(), "kilocode", "", true, false); !mapsEqualJSON(got, want) {
		t.Fatalf("override pricing %v", got)
	}
	if seen[len(seen)-1].url != "https://proxy.example.com/kilo/models" {
		t.Fatalf("override url %v", seen)
	}
	pricingPayloadStub(t, nil, nil)
	if got := GetPricingForProvider(context.Background(), "kilocode", "", true, false); len(got) != 0 {
		t.Fatalf("endpoint down gives %v", got)
	}
}

func TestDeepInfraProfileRegisteredWithAliasAndAux(t *testing.T) {
	env := apiKeyProvidersEnv(t)
	profile, ok := ProfileFor("deepinfra")
	if !ok || profile.Name != "deepinfra" || profile.AuthType != AuthTypeAPIKey {
		t.Fatalf("profile %+v %v", profile, ok)
	}
	if alias, ok := ProfileFor("deep-infra"); !ok || alias.Name != profile.Name {
		t.Fatalf("alias profile %+v %v", alias, ok)
	}
	if got, err := resolveProvider(env, "deep-infra"); err != nil || got != "deepinfra" {
		t.Fatalf("resolve_provider %q %v", got, err)
	}
	if reg := authRegistryGet(context.Background(), "deepinfra"); reg == nil || reg.BaseURL != profile.BaseURL {
		t.Fatalf("auth registry entry %+v", reg)
	}
	if !slices.ContainsFunc(CanonicalProviders(), func(e CanonicalProviderEntry) bool { return e.Slug == "deepinfra" }) {
		t.Fatal("deepinfra missing from the canonical providers")
	}
	if !config.OptionalEnvVars["DEEPINFRA_API_KEY"].Password || config.OptionalEnvVars["DEEPINFRA_BASE_URL"].Password {
		t.Fatal("deepinfra env var password flags")
	}
	if auxGetAuxModelForProvider(context.Background(), "deepinfra") == "" {
		t.Fatal("deepinfra aux model")
	}
	if profile.FallbackModels == nil {
		t.Fatal("deepinfra fallback models")
	}
}

func TestRuntimeAlibabaRegionalAndTokenPlan(t *testing.T) {
	for _, tc := range []struct{ requested, env, key, base string }{
		{"alibaba-cn", "DASHSCOPE_API_KEY", "ds-key", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"alibaba-coding-plan-cn", "ALIBABA_CODING_PLAN_API_KEY", "acp-key", "https://coding.dashscope.aliyuncs.com/v1"},
		{"alibaba-token-plan", "ALIBABA_TOKEN_PLAN_API_KEY", "atp-key", "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"},
		{"alibaba-token-plan-cn", "ALIBABA_TOKEN_PLAN_API_KEY", "atp-key", "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"},
	} {
		t.Run(tc.requested, func(t *testing.T) {
			r := testResolver(t, nil, &fakeCreds{}, "")
			clearProviderEnv(t)
			t.Setenv(tc.env, tc.key)
			rt := mustResolve(t, r, tc.requested, "", "", "")
			if rt.Provider != tc.requested || rt.APIMode != ModeChatCompletions || rt.APIKey != tc.key || rt.BaseURL != tc.base {
				t.Fatalf("runtime %+v", rt)
			}
		})
	}
}
