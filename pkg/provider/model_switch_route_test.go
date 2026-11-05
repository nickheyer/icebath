package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

var switchAccepted = ModelVerdict{Accepted: true, Persist: true, Recognized: true}

type switchFakes struct {
	validatedAs []string
	headers     []map[string]string
}

// Replaces metadata, usage and validation seams of the switch pipeline with offline answers
func fakeSwitchPipeline(t *testing.T, fakeValidation bool) *switchFakes {
	t.Helper()
	fakes := &switchFakes{}
	savedValidate, savedCaps, savedInfo, savedUsage := switchValidateModelFn, switchCapabilitiesFn, switchModelInfoFn, switchUsageNoticeFn
	savedCache, savedTime := modelsDevCache, modelsDevCacheTime
	modelsDevCache, modelsDevCacheTime = map[string]any{}, time.Now()
	if fakeValidation {
		switchValidateModelFn = func(_ context.Context, _, provider string, opts ValidateModelOptions) ModelVerdict {
			fakes.validatedAs = append(fakes.validatedAs, provider)
			fakes.headers = append(fakes.headers, opts.Headers)
			return switchAccepted
		}
	}
	switchCapabilitiesFn = func(context.Context, string, string, bool) (ModelCapabilities, bool) {
		return ModelCapabilities{}, false
	}
	switchModelInfoFn = func(context.Context, string, string, bool) (ModelsDevEntry, bool) { return ModelsDevEntry{}, false }
	switchUsageNoticeFn = func(context.Context, string, string, bool, string) string { return "" }
	t.Cleanup(func() {
		switchValidateModelFn, switchCapabilitiesFn, switchModelInfoFn, switchUsageNoticeFn = savedValidate, savedCaps, savedInfo, savedUsage
		modelsDevCache, modelsDevCacheTime = savedCache, savedTime
	})
	return fakes
}

// Answers every runtime resolution with the given runtime
func fakeSwitchRuntime(t *testing.T, rt *Runtime, err error) {
	t.Helper()
	saved := switchResolveRuntimeFn
	switchResolveRuntimeFn = func(context.Context, string, string, string, string) (*Runtime, error) { return rt, err }
	t.Cleanup(func() { switchResolveRuntimeFn = saved })
}

// Fixes catalog listings and static provider detection for routing
func fakeSwitchRouting(t *testing.T, catalog []string, detected [2]string) {
	t.Helper()
	savedList, savedDetect := listProviderModelsFn, switchDetectProviderFn
	listProviderModelsFn = func(context.Context, string) []string { return catalog }
	switchDetectProviderFn = func(context.Context, string, string) (string, string, bool) {
		return detected[0], detected[1], detected[0] != ""
	}
	t.Cleanup(func() { listProviderModelsFn, switchDetectProviderFn = savedList, savedDetect })
}

func mustSwitch(t *testing.T, ctx context.Context, opts SwitchModelOptions) ModelSwitchResult {
	t.Helper()
	res, err := SwitchModel(ctx, opts)
	if err != nil {
		t.Fatalf("switch %q: %v", opts.RawInput, err)
	}
	return res
}

func TestSwitchModelAcceptsExplicitBareCustomCurrentEndpoint(t *testing.T) {
	ctx, _ := switchAliasHome(t, "")
	fakeSwitchPipeline(t, true)
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "gpt-4o-mini", CurrentProvider: "custom",
		CurrentBaseURL: "https://www.ccsub.net/v1", CurrentAPIKey: "sk-test", ExplicitProvider: "custom",
		UserProviders: map[string]any{}, CustomProviders: []any{}})
	if !res.Success || res.TargetProvider != "custom" || res.NewModel != "gpt-4o-mini" || res.BaseURL != "https://www.ccsub.net/v1" || res.APIKey != "sk-test" {
		t.Fatalf("result %+v", res)
	}
}

func TestSwitchToBareCustomFromAnotherProviderResolvesTheConfiguredEndpoint(t *testing.T) {
	ctx, _ := switchAliasHome(t, "model:\n  default: qwen3:8b\n  provider: custom\n  base_url: http://127.0.0.1:11434/v1\n")
	fakeSwitchPipeline(t, true)
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "qwen3:8b", CurrentProvider: "openrouter",
		CurrentBaseURL: "https://openrouter.ai/api/v1", CurrentAPIKey: "sk-openrouter", ExplicitProvider: "custom",
		UserProviders: map[string]any{}, CustomProviders: []any{}})
	if !res.Success || res.TargetProvider != "custom" || res.BaseURL != "http://127.0.0.1:11434/v1" || res.APIKey != "no-key-required" {
		t.Fatalf("result %+v", res)
	}
}

func TestSwitchToBareCustomWithNoConfiguredEndpointKeepsTheCurrentOne(t *testing.T) {
	ctx, _ := switchAliasHome(t, "model:\n  default: m\n  provider: anthropic\n")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-env")
	fakeSwitchPipeline(t, true)
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "m2", CurrentProvider: "anthropic",
		CurrentBaseURL: "https://api.anthropic.com", CurrentAPIKey: "sk-ant", ExplicitProvider: "custom",
		UserProviders: map[string]any{}, CustomProviders: []any{}})
	if !res.Success || res.BaseURL != "https://api.anthropic.com" || res.APIKey != "sk-ant" {
		t.Fatalf("result %+v", res)
	}
}

func TestOpenrouterMirrorReadNeverRaisesWithoutASecretScope(t *testing.T) {
	t.Setenv("OPENROUTER_BASE_URL", "https://mirror.example.com/v1")
	SetMultiplexActive(true)
	defer SetMultiplexActive(false)
	if got := openrouterMirrorBaseURL(context.Background()); got != "" {
		t.Fatalf("mirror %q", got)
	}
}

func TestSwitchToBareCustomIgnoresAnOpenrouterMirror(t *testing.T) {
	ctx, _ := switchAliasHome(t, "model:\n  default: m\n  provider: custom\n")
	t.Setenv("OPENROUTER_BASE_URL", "https://mirror.example.com/v1")
	fakeSwitchPipeline(t, true)
	opts := SwitchModelOptions{RawInput: "m2", CurrentProvider: "anthropic", CurrentBaseURL: "https://api.anthropic.com",
		CurrentAPIKey: "sk-ant", ExplicitProvider: "custom", UserProviders: map[string]any{}, CustomProviders: []any{}}
	res := mustSwitch(t, ctx, opts)
	if !res.Success || res.BaseURL != "https://api.anthropic.com" || res.APIKey != "sk-ant" {
		t.Fatalf("result %+v", res)
	}
	t.Setenv("CUSTOM_BASE_URL", "https://mirror.example.com/v1")
	if configured := mustSwitch(t, ctx, opts); configured.BaseURL != "https://mirror.example.com/v1" {
		t.Fatalf("configured %+v", configured)
	}
}

func TestSwitchModelDoesNotSendOllamaHeadersToUnrelatedCustomEndpoint(t *testing.T) {
	ctx, _ := switchAliasHome(t, "providers:\n  ollama:\n    base_url: https://trusted-ollama.example:11434\n")
	fakes := fakeSwitchPipeline(t, true)
	fakeSwitchRouting(t, nil, [2]string{})
	var seen []map[string]string
	savedHeaders, savedNative := switchOllamaHeadersFn, switchOllamaNativeCatalogFn
	switchOllamaHeadersFn = func(context.Context) map[string]string {
		return map[string]string{"Authorization": "Bearer configured-ollama-secret"}
	}
	switchOllamaNativeCatalogFn = func(_ context.Context, _, _ string, headers map[string]string) bool {
		seen = append(seen, headers)
		return true
	}
	t.Cleanup(func() { switchOllamaHeadersFn, switchOllamaNativeCatalogFn = savedHeaders, savedNative })
	fakeSwitchRuntime(t, &Runtime{APIKey: "custom-key", BaseURL: "https://attacker.example:11434/v1", APIMode: ModeChatCompletions}, nil)
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "new-model", CurrentProvider: "custom",
		CurrentBaseURL: "https://attacker.example:11434/v1", CurrentAPIKey: "custom-key",
		UserProviders: map[string]any{}, CustomProviders: []any{}})
	if !res.Success {
		t.Fatalf("result %+v", res)
	}
	if len(seen) != 1 || seen[0] == nil || len(seen[0]) != 0 {
		t.Fatalf("native detection headers %#v", seen)
	}
	if len(fakes.headers) != 1 || fakes.headers[0] != nil {
		t.Fatalf("validation headers %#v", fakes.headers)
	}
}

func TestPickerSelectionResolvesNamedCustomProviderModelID(t *testing.T) {
	ctx, _ := switchAliasHome(t, "")
	fakeSwitchPipeline(t, true)
	fakeSwitchRuntime(t, &Runtime{APIKey: "test-key", BaseURL: "https://token.sensenova.cn/v1", APIMode: ModeChatCompletions}, nil)
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "sensenova/deepseek-v4-flash", CurrentProvider: "openai-codex",
		ExplicitProvider: "custom:sensenova", UserProviders: map[string]any{}, CustomProviders: []any{map[string]any{
			"name": "sensenova", "base_url": "https://token.sensenova.cn/v1",
			"models": []any{map[string]any{"id": "deepseek-v4-flash", "name": "deepseek-v4-flash"}}}}})
	if !res.Success || res.TargetProvider != "custom:sensenova" || res.NewModel != "deepseek-v4-flash" {
		t.Fatalf("result %+v", res)
	}
}

func TestSameProviderSwitchOnSessionOnlyCustomEndpointKeepsEndpoint(t *testing.T) {
	ctx, _ := switchAliasHome(t, "model:\n  provider: openrouter\n  default: x\n")
	fakeSwitchPipeline(t, true)
	fakeSwitchRouting(t, nil, [2]string{})
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "m-b", CurrentProvider: "custom",
		CurrentBaseURL: "http://10.0.0.5:8000/v1", CurrentAPIKey: "session-secret"})
	if !res.Success || res.BaseURL != "http://10.0.0.5:8000/v1" || res.APIKey != "session-secret" {
		t.Fatalf("result %+v", res)
	}
}

func TestLegacySentinelCatalogStillResolves(t *testing.T) {
	var catalog []string
	models := map[string]any{"__discovered_model_catalog__": true}
	for i := 1; i <= 8; i++ {
		id := "omlx-model-" + string(rune('0'+i))
		catalog = append(catalog, id)
		models[id] = map[string]any{}
	}
	entry := map[string]any{"name": "Local MLX", "base_url": "http://127.0.0.1:8000/v1", "model": "omlx-model-1", "models": models}
	if !entryModelsDiscovered(entry) {
		t.Fatal("legacy sentinel not read as a discovered catalog")
	}
	if modelsConfigIsAllowlist(models, entryModelsDiscovered(entry)) {
		t.Fatal("legacy sentinel read as an allowlist")
	}
	if got := declaredModelIDs(models); !reflect.DeepEqual(got, catalog) {
		t.Fatalf("declared %v", got)
	}
}

// Runs a switch from an aggregator session with no alias, catalog or detection hits
func runVariantSwitch(t *testing.T, raw, current string) string {
	t.Helper()
	ctx, _ := switchAliasHome(t, "")
	fakeSwitchPipeline(t, true)
	fakeSwitchRouting(t, []string{}, [2]string{})
	fakeSwitchRuntime(t, &Runtime{APIKey: "test", APIMode: ModeChatCompletions}, nil)
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: raw, CurrentProvider: current})
	if !res.Success {
		t.Fatalf("switch_model failed: %s", res.ErrorMessage)
	}
	return res.NewModel
}

func TestSlashFormatPreservesVariantTag(t *testing.T) {
	for _, model := range []string{"nvidia/nemotron-3-super-120b-a12b:free", "anthropic/claude-sonnet-4.6:extended", "meta-llama/llama-4-maverick:fast"} {
		if got := runVariantSwitch(t, model, "openrouter"); got != model {
			t.Fatalf("%s became %s", model, got)
		}
	}
}

func TestLegacyColonFormatConvertsToSlash(t *testing.T) {
	if got := runVariantSwitch(t, "nvidia:nemotron-3-super-120b-a12b", "openrouter"); got != "nvidia/nemotron-3-super-120b-a12b" {
		t.Fatalf("got %s", got)
	}
}

func TestKnownProviderColonMatchesSlashForm(t *testing.T) {
	colon := runVariantSwitch(t, "Alibaba:qwen3.6-plus", "alibaba")
	slash := runVariantSwitch(t, "Alibaba/qwen3.6-plus", "alibaba")
	if colon != slash {
		t.Fatalf("colon %s slash %s", colon, slash)
	}
}

func TestNonProviderLeftSideKeepsColon(t *testing.T) {
	if got := runVariantSwitch(t, "qwen3.5:4b", "alibaba"); got != "qwen3.5:4b" {
		t.Fatalf("got %s", got)
	}
}

func TestUnlistedIDOnUserProviderIsKeptVerbatim(t *testing.T) {
	listing := []string{"deepseek-v4-flash-0731", "deepseek-v4-flash", "deepseek-v4-pro"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data []map[string]string
		for _, m := range listing {
			data = append(data, map[string]string{"id": m})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()
	endpoint := srv.URL + "/v1"
	for _, explicit := range []string{"hyper", "custom:hyper"} {
		ctx, _ := switchAliasHome(t, "model:\n  provider: custom:hyper\n  default: deepseek-v4-flash-0731\n"+
			"providers:\n  hyper:\n    base_url: "+endpoint+"\n    api_key_env: HYPER_KEY\n")
		t.Setenv("HYPER_KEY", "test-key-12345")
		fakeSwitchPipeline(t, false)
		cfg := modelsConfig(ctx)
		var custom []any
		for _, entry := range CompatibleCustomProviders(cfg) {
			custom = append(custom, entry)
		}
		providers, _ := cfg["providers"].(map[string]any)
		res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "deepseek-v4.1-flash", ExplicitProvider: explicit,
			CurrentProvider: "custom:hyper", CurrentBaseURL: endpoint, CurrentAPIKey: "test-key-12345",
			UserProviders: providers, CustomProviders: custom})
		if !res.Success || res.NewModel != "deepseek-v4.1-flash" || res.BaseURL != endpoint || !strings.Contains(res.WarningMessage, "not found") {
			t.Fatalf("%s: result %+v", explicit, res)
		}
	}
}
