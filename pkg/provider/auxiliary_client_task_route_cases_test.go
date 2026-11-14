package provider

import (
	"context"
	"reflect"
	"testing"
)

// Temp home whose config.yaml holds the two MoA presets the shared resolution tests read
func auxWriteMoaConfig(t *testing.T, defaultPreset, extraYAML string) context.Context {
	t.Helper()
	yaml := "moa:\n  default_preset: " + defaultPreset + "\n  presets:\n" +
		"    opus-gpt:\n      enabled: true\n      reference_models:\n        - provider: openrouter\n          model: openai/gpt-5.5\n" +
		"      aggregator:\n        provider: openrouter\n        model: anthropic/claude-opus-4.8\n" +
		"    nous-mix:\n      enabled: true\n      reference_models:\n        - provider: nous\n          model: hermes-4-70b\n" +
		"      aggregator:\n        provider: nous\n        model: hermes-4-405b\n"
	return auxResolveHome(t, yaml+extraYAML)
}

func TestResolveTaskProviderModelExplicitBaseUrlPreservesFirstClassProviderIdentity(t *testing.T) {
	ctx := auxResolveHome(t, "")
	for _, provider := range []string{"anthropic", "minimax-oauth", "nous", "openai-codex", "qwen-oauth", "xai-oauth"} {
		route := auxResolveTaskProviderModel(ctx, "moa_reference", provider, "test-model", "https://provider.example/v1", "resolved-token")
		if route.Provider != provider || route.Model != "test-model" || route.BaseURL != "https://provider.example/v1" || route.APIKey != "resolved-token" || route.APIMode != "" {
			t.Fatalf("%s resolved %+v", provider, route)
		}
	}
}

func TestResolveTaskProviderModelExplicitProviderAdoptsConfiguredTaskEndpoint(t *testing.T) {
	ctx := auxResolveHome(t, "auxiliary:\n  vision:\n    provider: custom\n    model: meta/llama-3.2-11b-vision-instruct\n    base_url: https://integrate.api.nvidia.com/v1\n    api_key: nvapi-secret\n")
	route := auxResolveTaskProviderModel(ctx, "vision", "custom", "meta/llama-3.2-11b-vision-instruct", "", nil)
	if route.Provider != "custom" || route.BaseURL != "https://integrate.api.nvidia.com/v1" || route.APIKey != "nvapi-secret" || route.Model != "meta/llama-3.2-11b-vision-instruct" || route.APIMode != "" {
		t.Fatalf("resolved %+v", route)
	}
}

func TestResolveTaskProviderModelExplicitProviderMoaUnwrapsToAggregator(t *testing.T) {
	ctx := auxResolveHome(t, "moa:\n  presets:\n    opus-gpt:\n      aggregator:\n        provider: openrouter\n        model: anthropic/claude-opus-4.8\n")
	route := auxResolveTaskProviderModel(ctx, "title_generation", "moa", "opus-gpt", "moa://local", "moa-virtual-provider")
	if route.Provider != "openrouter" || route.Model != "anthropic/claude-opus-4.8" || route.BaseURL != "" || route.APIKey != nil {
		t.Fatalf("resolved %+v", route)
	}
}

func TestResolveTaskProviderModelConfigProviderMoaUnwrapsToAggregator(t *testing.T) {
	ctx := auxResolveHome(t, "moa:\n  presets:\n    opus-gpt:\n      aggregator:\n        provider: anthropic\n        model: claude-opus-4.8\nauxiliary:\n  title_generation:\n    provider: moa\n    model: opus-gpt\n")
	route := auxResolveTaskProviderModel(ctx, "title_generation", "", "", "", nil)
	if route.Provider != "anthropic" || route.Model != "claude-opus-4.8" || route.BaseURL != "" || route.APIKey != nil {
		t.Fatalf("resolved %+v", route)
	}
}

func TestResolveTaskProviderModelProviderMoaFallsBackToLiteralWhenPresetResolutionFails(t *testing.T) {
	ctx := auxResolveHome(t, "moa: {}\n")
	route := auxResolveTaskProviderModel(ctx, "title_generation", "moa", "gone-preset", "", nil)
	if route.Provider != "moa" || route.Model != "gone-preset" {
		t.Fatalf("resolved %+v", route)
	}
}

func TestResolveTaskProviderModelExplicitModelAutoSentinelIsNormalized(t *testing.T) {
	ctx := auxResolveHome(t, "")
	route := auxResolveTaskProviderModel(ctx, nil, "anthropic", "auto", "", nil)
	if route.Provider != "anthropic" || route.Model != "" {
		t.Fatalf("resolved %+v", route)
	}
}

func TestMoaAggregatorSharedResolutionRealConfigExplicitTaskProviderMoa(t *testing.T) {
	ctx := auxWriteMoaConfig(t, "opus-gpt", "auxiliary:\n  title_generation:\n    provider: moa\n    model: opus-gpt\n")
	route := auxResolveTaskProviderModel(ctx, "title_generation", "", "", "", nil)
	if route.Provider != "openrouter" || route.Model != "anthropic/claude-opus-4.8" || route.BaseURL != "" || route.APIKey != nil {
		t.Fatalf("resolved %+v", route)
	}
}

func TestMoaAggregatorSharedResolutionMainAgentFallbackUsesAggregatorForMoaMain(t *testing.T) {
	ctx := auxWriteMoaConfig(t, "opus-gpt", "")
	auxClearUnhealthy(t)
	prevProvider, prevModel := auxReadMainProvider, auxReadMainModel
	auxReadMainProvider = func(context.Context) string { return "moa" }
	auxReadMainModel = func(context.Context) string { return "opus-gpt" }
	t.Cleanup(func() { auxReadMainProvider, auxReadMainModel = prevProvider, prevModel })
	mock := &auxCallFakeClient{base: "https://openrouter.ai/api/v1"}
	var gotProvider, gotModel string
	auxFakeResolveProviderClient(t, func(provider, model string, _ ProviderClientOptions) (AuxClient, string) {
		gotProvider, gotModel = provider, model
		return mock, "anthropic/claude-opus-4.8"
	})
	client, model, label, err := auxTryMainAgentModelFallback(ctx, "anthropic", "compression", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if client != AuxClient(mock) || model != "anthropic/claude-opus-4.8" || label != "main-agent(openrouter)" {
		t.Fatalf("client %v model %q label %q", client, model, label)
	}
	if gotProvider != "openrouter" || gotModel != "anthropic/claude-opus-4.8" {
		t.Fatalf("resolved %q %q", gotProvider, gotModel)
	}
}

func TestBuildCallKwargsMaxTokensKeepsMaxTokensOnAnthropicWire(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	for _, c := range [][3]string{
		{"minimax", "minimax-m2", "https://api.minimax.io/v1"},
		{"custom", "claude", "https://proxy.example.com/anthropic/v1"},
	} {
		kwargs, err := auxBuildCallKwargs(ctx, c[0], c[1], auxUserHi(), auxCallKwargsOptions{MaxTokens: 1234, BaseURL: c[2], Timeout: 120})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := kwargs["max_completion_tokens"]; ok || kwargs["max_tokens"] != 1234 {
			t.Fatalf("%s kwargs %v", c[0], kwargs)
		}
	}
}

func TestBuildCallKwargsMaxTokensMoaTaskSendsMaxTokensOnOpenaiCompatible(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	for _, c := range [][4]string{
		{"zai", "glm-5.2", "https://api.z.ai/api/coding/paas/v4", "max_tokens"},
		{"openrouter", "deepseek/deepseek-v4-flash:nitro", "https://openrouter.ai/api/v1", "max_tokens"},
		{"copilot", "gpt-5.5", "https://api.githubcopilot.com", "max_completion_tokens"},
		{"nous", "hermes-4", "https://inference-api.nousresearch.com/v1", "max_tokens"},
	} {
		kwargs, err := auxBuildCallKwargs(ctx, c[0], c[1], auxUserHi(), auxCallKwargsOptions{MaxTokens: 800, BaseURL: c[2], Task: "moa_reference", Timeout: 120})
		if err != nil {
			t.Fatal(err)
		}
		if kwargs[c[3]] != 800 {
			t.Fatalf("%s kwargs %v", c[0], kwargs)
		}
	}
}

func TestBuildCallKwargsMaxTokensMoaTaskExactMatch(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	build := func(task string) map[string]any {
		kwargs, err := auxBuildCallKwargs(ctx, "zai", "glm-5.2", auxUserHi(), auxCallKwargsOptions{MaxTokens: 500, BaseURL: "https://api.z.ai/api/coding/paas/v4", Task: task, Timeout: 120})
		if err != nil {
			t.Fatal(err)
		}
		return kwargs
	}
	if kw := build("moa_reference"); kw["max_tokens"] != 500 {
		t.Fatalf("moa_reference kwargs %v", kw)
	}
	for _, task := range []string{"moa_aggregator", "moa_custom_future"} {
		if kw := build(task); kw["max_tokens"] != nil {
			t.Fatalf("%s kwargs %v", task, kw)
		}
	}
}

func TestNousTagsScopingTagsInjectedWhenProviderIsNous(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kwargs, err := auxBuildCallKwargs(ctx, "nous", "hermes-4", auxUserHi(), auxCallKwargsOptions{Timeout: 120})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := kwargs["extra_body"].(map[string]any)
	var got []string
	switch tags := body["tags"].(type) {
	case []string:
		got = tags
	case []any:
		for _, tag := range tags {
			got = append(got, tag.(string))
		}
	}
	if !reflect.DeepEqual(got, nousPortalTags(ctx, "")) {
		t.Fatalf("tags %v", body["tags"])
	}
}

func TestNousTagsScopingTagsNotInjectedForNonNousProviderWhenMainIsNous(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	scoped, _ := ScopedRuntimeMain(ctx, map[string]any{
		"provider": "nous", "model": "Hermes-4",
		"base_url": "https://inference-api.nousresearch.com/v1", "api_key": "nous-key",
	})
	kwargs, err := auxBuildCallKwargs(scoped, "gemini", "gemini-2.5-flash", auxUserHi(), auxCallKwargsOptions{Timeout: 120})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kwargs["extra_body"]; ok {
		t.Fatalf("extra_body present %v", kwargs["extra_body"])
	}
}

func TestNormalizeAuxProviderMainProviderOpencodeResolvesAnAuxClient(t *testing.T) {
	ctx := auxResolveHome(t, "")
	auxIsolateClientCache(t)
	t.Setenv("OPENCODE_ZEN_API_KEY", "sk-test-not-real")
	aliasClient, aliasModel, err := ResolveProviderClient(ctx, "opencode", "glm-5.3", ProviderClientOptions{Task: "approval"})
	if err != nil {
		t.Fatal(err)
	}
	canonClient, canonModel, err := ResolveProviderClient(ctx, "opencode-zen", "glm-5.3", ProviderClientOptions{Task: "approval"})
	if err != nil {
		t.Fatal(err)
	}
	if canonClient == nil || aliasClient == nil {
		t.Fatalf("alias %v canonical %v", aliasClient, canonClient)
	}
	if aliasClient.BaseURL() != canonClient.BaseURL() || aliasModel != canonModel {
		t.Fatalf("alias %s %s canonical %s %s", aliasClient.BaseURL(), aliasModel, canonClient.BaseURL(), canonModel)
	}
}

func TestNormalizeAuxProviderCoversEveryAliasTheMainPathResolves(t *testing.T) {
	ctx := auxResolveHome(t, "")
	for alias, canonical := range authProviderAliases {
		if got := auxNormalizeAuxProvider(ctx, alias); got != canonical {
			t.Fatalf("%s normalized to %q want %q", alias, got, canonical)
		}
	}
}
