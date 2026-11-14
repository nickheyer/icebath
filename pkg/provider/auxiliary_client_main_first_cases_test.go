package provider

import (
	"context"
	"sync"
	"testing"
)

// One recorded resolve_provider_client call
type auxResolveCall struct {
	Provider, Model string
	Opts            ProviderClientOptions
}

// Records every resolve call and answers with the client and the model the chooser returns
func auxRecordResolve(t *testing.T, client AuxClient, choose func(model string) string) *[]auxResolveCall {
	t.Helper()
	var mu sync.Mutex
	calls := &[]auxResolveCall{}
	prev := auxResolveProviderClientFn
	t.Cleanup(func() { auxResolveProviderClientFn = prev })
	auxResolveProviderClientFn = func(_ context.Context, provider, model string, opts ProviderClientOptions) (AuxClient, string, error) {
		mu.Lock()
		*calls = append(*calls, auxResolveCall{Provider: provider, Model: model, Opts: opts})
		mu.Unlock()
		if client == nil {
			return nil, "", nil
		}
		return client, choose(model), nil
	}
	return calls
}

// Swaps the main provider and model readers the way hermes tests patch _read_main_provider and _read_main_model
func auxSwapMainReaders(t *testing.T, provider, model string) {
	t.Helper()
	prevProvider, prevModel := auxReadMainProvider, auxReadMainModel
	t.Cleanup(func() { auxReadMainProvider, auxReadMainModel = prevProvider, prevModel })
	auxReadMainProvider = func(context.Context) string { return provider }
	auxReadMainModel = func(context.Context) string { return model }
}

// Clears the unhealthy provider memo so every provider counts as healthy
func auxAllProvidersHealthy(t *testing.T) {
	t.Helper()
	auxUnhealthyMu.Lock()
	clear(auxUnhealthyUntil)
	auxUnhealthyMu.Unlock()
}

// Swaps the catalog fast model lookup for a fixed answer
func auxFakeFastModel(t *testing.T, model string) {
	t.Helper()
	prev := auxFastModelFromCatalogFn
	t.Cleanup(func() { auxFastModelFromCatalogFn = prev })
	auxFastModelFromCatalogFn = func(context.Context, string) string { return model }
}

// Swaps the task route resolver for a fixed route the way hermes tests patch _resolve_task_provider_model
func auxFakeTaskRoute(t *testing.T, route auxTaskRoute) {
	t.Helper()
	prev := auxResolveTaskProviderModelFn
	t.Cleanup(func() { auxResolveTaskProviderModelFn = prev })
	auxResolveTaskProviderModelFn = func(context.Context, any, string, string, string, any) auxTaskRoute { return route }
}

// Stubs the Nous network boundary with the Portal's tier aware slots and records the vision flag
func auxFakeNousPortal(t *testing.T, seen *map[string]bool) AuxClient {
	t.Helper()
	nousClient := &auxCallFakeClient{base: "https://inference-api.nousresearch.com/v1", key: "jwt-test",
		create: func(context.Context, map[string]any) (any, error) { return auxCallOK("ok"), nil }}
	prev := auxTryNousFn
	t.Cleanup(func() { auxTryNousFn = prev })
	auxTryNousFn = func(_ context.Context, vision bool) (AuxClient, string, error) {
		(*seen)["vision"] = vision
		if vision {
			return nousClient, "stepfun/step-3.7-flash:free", nil
		}
		return nousClient, "tencent/hy3:free", nil
	}
	return nousClient
}

func TestResolveAutoMainFirstTitleGenerationAutoHonorsMainModel(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxAllProvidersHealthy(t)
	auxFakeFastModel(t, "gemini-3-flash")
	mainModel := "deepseek-v4-flash-free"
	mock := &auxCallFakeClient{base: "https://opencode.ai/zen/v1", key: "k"}
	calls := auxRecordResolve(t, mock, func(string) string { return mainModel })
	client, model, _, err := auxResolveAutoRoute(ctx, map[string]any{"provider": "opencode-zen", "model": mainModel}, "title_generation")
	if err != nil {
		t.Fatal(err)
	}
	if client != AuxClient(mock) || model != mainModel {
		t.Fatalf("client %v model %q", client, model)
	}
	if len(*calls) == 0 || (*calls)[len(*calls)-1].Provider != "opencode-zen" || (*calls)[len(*calls)-1].Model != mainModel {
		t.Fatalf("calls %+v", *calls)
	}
}

func TestResolveAutoMainFirstTitleGenerationCanOptIntoProviderFastModel(t *testing.T) {
	ctx := auxMainTestHome(t, "auxiliary:\n  title_generation:\n    prefer_fast_model: true\n")
	auxAllProvidersHealthy(t)
	auxFakeFastModel(t, "gemini-3-flash")
	mock := &auxCallFakeClient{base: "https://opencode.ai/zen/v1", key: "k"}
	auxRecordResolve(t, mock, func(model string) string { return model })
	client, model, _, err := auxResolveAutoRoute(ctx, map[string]any{"provider": "opencode-zen", "model": "deepseek-v4-flash-free"}, "title_generation")
	if err != nil {
		t.Fatal(err)
	}
	if client != AuxClient(mock) || model != "gemini-3-flash" {
		t.Fatalf("client %v model %q", client, model)
	}
}

func TestResolveAutoMainFirstMoaMainResolvesAuxToAggregator(t *testing.T) {
	ctx := auxMainTestHome(t, "moa:\n  default_preset: opus-gpt\n  presets:\n    opus-gpt:\n      enabled: true\n"+
		"      reference_models:\n        - provider: openrouter\n          model: openai/gpt-5.5\n"+
		"      aggregator:\n        provider: openrouter\n        model: anthropic/claude-opus-4.8\n")
	auxAllProvidersHealthy(t)
	mock := &auxCallFakeClient{base: "https://openrouter.ai/api/v1", key: "k"}
	calls := auxRecordResolve(t, mock, func(string) string { return "anthropic/claude-opus-4.8" })
	runtime := map[string]any{"provider": "moa", "model": "opus-gpt", "base_url": "moa://local", "api_key": "moa-virtual-provider", "api_mode": "chat_completions"}
	client, _, _, err := auxResolveAutoRoute(ctx, runtime, "title_generation")
	if err != nil {
		t.Fatal(err)
	}
	if client != AuxClient(mock) {
		t.Fatalf("client %v", client)
	}
	last := (*calls)[len(*calls)-1]
	if last.Provider != "openrouter" || last.Model != "anthropic/claude-opus-4.8" || last.Opts.ExplicitBaseURL != "" {
		t.Fatalf("call %+v", last)
	}
}

func TestResolveAutoMainFirstMainUnavailableUsesTaskFallbackChainBeforeBuiltinChain(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxAllProvidersHealthy(t)
	auxSwapFallbackSeams(t)
	auxSwapMainReaders(t, "nvidia", "qwen/qwen3.5-122b-a10b")
	auxRecordResolve(t, nil, nil)
	taskClient := &auxCallFakeClient{base: "https://openrouter.ai/api/v1", key: "k"}
	var chainCalls [][3]string
	auxTryConfiguredFallbackChainFn = func(_ context.Context, task, failed, reason, _, _ string, _ FailureScope) (AuxClient, string, string, error) {
		chainCalls = append(chainCalls, [3]string{task, failed, reason})
		return taskClient, "task-free-model", "fallback_chain[0](openrouter)", nil
	}
	auxTryMainFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		t.Error("main fallback chain called")
		return nil, "", "", nil
	}
	auxProviderChainFn = func() []auxProviderChainEntry {
		t.Error("builtin chain called")
		return nil
	}
	client, model, _, err := auxResolveAutoRoute(ctx, nil, "title_generation")
	if err != nil {
		t.Fatal(err)
	}
	if client != AuxClient(taskClient) || model != "task-free-model" {
		t.Fatalf("client %v model %q", client, model)
	}
	if len(chainCalls) != 1 || chainCalls[0] != [3]string{"title_generation", "nvidia", "main provider unavailable"} {
		t.Fatalf("chain calls %v", chainCalls)
	}
}

func TestResolveAutoMainFirstResolveProviderAutoReturnsRuntimeModelNotStaleConfigDefault(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	prevModel := auxReadMainModel
	t.Cleanup(func() { auxReadMainModel = prevModel })
	auxReadMainModel = func(context.Context) string {
		t.Error("stale config model read")
		return "claude-opus-4-8"
	}
	runtimeClient := &auxCallFakeClient{base: "https://chatgpt.com/backend-api/codex", key: ""}
	autoCalls := 0
	prevAuto := auxResolveAutoRouteFn
	t.Cleanup(func() { auxResolveAutoRouteFn = prevAuto })
	auxResolveAutoRouteFn = func(context.Context, map[string]any, string) (AuxClient, string, string, error) {
		autoCalls++
		return runtimeClient, "gpt-5.5", "openai-codex", nil
	}
	runtime := map[string]any{"provider": "openai-codex", "model": "gpt-5.5", "base_url": "", "api_key": "", "api_mode": "codex_responses"}
	client, model, err := ResolveProviderClient(ctx, "auto", "", ProviderClientOptions{MainRuntime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	if client != AuxClient(runtimeClient) || model != "gpt-5.5" || autoCalls != 1 {
		t.Fatalf("client %v model %q auto calls %d", client, model, autoCalls)
	}
}

func TestResolveAutoMainFirstRuntimeBaseUrlPassedForNamedApiKeyProvider(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxAllProvidersHealthy(t)
	auxSwapMainReaders(t, "openrouter", "config-model")
	tokenPlanURL := "https://token-plan-sgp.xiaomimimo.com/v1"
	calls := auxRecordResolve(t, &auxCallFakeClient{base: tokenPlanURL, key: "tp-test-key"}, func(string) string { return "mimo-v2.5-pro" })
	runtime := map[string]any{"provider": "xiaomi", "model": "mimo-v2.5-pro", "base_url": tokenPlanURL, "api_key": "tp-test-key", "api_mode": "chat_completions"}
	if _, _, _, err := auxResolveAutoRoute(ctx, runtime, ""); err != nil {
		t.Fatal(err)
	}
	last := (*calls)[len(*calls)-1]
	if last.Provider != "xiaomi" || last.Model != "mimo-v2.5-pro" || last.Opts.ExplicitBaseURL != tokenPlanURL || last.Opts.ExplicitAPIKey != "tp-test-key" || last.Opts.APIMode != "chat_completions" {
		t.Fatalf("call %+v", last)
	}
}

func TestResolveVisionMainFirstOpenrouterMainVisionUsesMainModel(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	auxSwapMainReaders(t, "openrouter", "anthropic/claude-sonnet-4.6")
	mock := &auxCallFakeClient{base: "https://openrouter.ai/api/v1", key: "or-key"}
	calls := auxRecordResolve(t, mock, func(string) string { return "anthropic/claude-sonnet-4.6" })
	auxFakeTaskRoute(t, auxTaskRoute{Provider: "auto"})
	provider, client, model, err := ResolveVisionProviderClient(ctx, "", "", "", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if provider != "openrouter" || client != AuxClient(mock) || model != "anthropic/claude-sonnet-4.6" {
		t.Fatalf("provider %q client %v model %q", provider, client, model)
	}
	if len(*calls) != 1 || (*calls)[0].Provider != "openrouter" || (*calls)[0].Model != "anthropic/claude-sonnet-4.6" || !(*calls)[0].Opts.IsVision {
		t.Fatalf("calls %+v", *calls)
	}
}

func TestResolveVisionMainFirstNousMainVisionUsesPortalPickNotTextChatModel(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	seen := map[string]bool{}
	nousClient := auxFakeNousPortal(t, &seen)
	auxSwapMainReaders(t, "nous", "tencent/hy3:free")
	auxFakeTaskRoute(t, auxTaskRoute{Provider: "auto"})
	provider, client, model, err := ResolveVisionProviderClient(ctx, "", "", "", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if provider != "nous" || client != nousClient || !seen["vision"] || model != "stepfun/step-3.7-flash:free" {
		t.Fatalf("provider %q client %v seen %v model %q", provider, client, seen, model)
	}
}

func TestResolveVisionMainFirstNousMainVisionHonoursExplicitVisionModel(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	seen := map[string]bool{}
	auxFakeNousPortal(t, &seen)
	auxSwapMainReaders(t, "nous", "tencent/hy3:free")
	auxFakeTaskRoute(t, auxTaskRoute{Provider: "auto", Model: "qwen/qwen3-vl-8b-instruct"})
	provider, _, model, err := ResolveVisionProviderClient(ctx, "", "", "", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if provider != "nous" || model != "qwen/qwen3-vl-8b-instruct" {
		t.Fatalf("provider %q model %q", provider, model)
	}
}

func TestResolveVisionMainFirstNousExplicitVisionProviderAlsoSkipsChatModel(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	seen := map[string]bool{}
	nousClient := auxFakeNousPortal(t, &seen)
	auxSwapMainReaders(t, "nous", "tencent/hy3:free")
	auxFakeTaskRoute(t, auxTaskRoute{Provider: "nous"})
	provider, client, model, err := ResolveVisionProviderClient(ctx, "", "", "", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if provider != "nous" || client != nousClient || model != "stepfun/step-3.7-flash:free" {
		t.Fatalf("provider %q client %v model %q", provider, client, model)
	}
}

func TestResolveVisionMainFirstNousTextAuxStillUsesMainChatModel(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	seen := map[string]bool{}
	auxFakeNousPortal(t, &seen)
	auxSwapMainReaders(t, "nous", "tencent/hy3:free")
	_, model, err := ResolveProviderClient(ctx, "nous", "", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if model != "tencent/hy3:free" {
		t.Fatalf("model %q", model)
	}
}

func TestResolveVisionCustomProviderCustomMainForwardsRuntimeEndpoint(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	ctx, _ = ScopedRuntimeMain(ctx, map[string]any{"base_url": "https://my.endpoint.example/v1", "api_key": "sk-runtime-key", "api_mode": "anthropic_messages"})
	auxSwapMainReaders(t, "custom", "claude-opus-4-8")
	auxFakeTaskRoute(t, auxTaskRoute{Provider: "auto"})
	mock := &auxCallFakeClient{base: "https://my.endpoint.example/v1", key: "sk-runtime-key"}
	calls := auxRecordResolve(t, mock, func(string) string { return "claude-opus-4-8" })
	provider, client, model, err := ResolveVisionProviderClient(ctx, "", "", "", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if provider != "custom" || client != AuxClient(mock) || model != "claude-opus-4-8" {
		t.Fatalf("provider %q client %v model %q", provider, client, model)
	}
	last := (*calls)[len(*calls)-1]
	if last.Opts.ExplicitBaseURL != "https://my.endpoint.example/v1" || last.Opts.ExplicitAPIKey != "sk-runtime-key" || !last.Opts.IsVision {
		t.Fatalf("call %+v", last)
	}
}

func TestResolveVisionCustomProviderCustomPrefixedMainForwardsRuntimeEndpoint(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	ctx, _ = ScopedRuntimeMain(ctx, map[string]any{"base_url": "https://named.example/v1", "api_key": "sk-named"})
	auxSwapMainReaders(t, "custom:copilot-gateway", "claude-opus-4-8")
	auxFakeTaskRoute(t, auxTaskRoute{Provider: "auto"})
	mock := &auxCallFakeClient{base: "https://named.example/v1", key: "sk-named"}
	calls := auxRecordResolve(t, mock, func(string) string { return "claude-opus-4-8" })
	provider, client, _, err := ResolveVisionProviderClient(ctx, "", "", "", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if provider != "custom:copilot-gateway" || client != AuxClient(mock) {
		t.Fatalf("provider %q client %v", provider, client)
	}
	last := (*calls)[len(*calls)-1]
	if last.Opts.ExplicitBaseURL != "https://named.example/v1" || last.Opts.ExplicitAPIKey != "sk-named" || !last.Opts.IsVision {
		t.Fatalf("call %+v", last)
	}
}

func TestResolveVisionCustomProviderCustomMainNoRuntimeFallsBackToConfiguredEndpoint(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	ctx, _ = ScopedRuntimeMain(ctx, map[string]any{})
	auxSwapMainReaders(t, "custom", "claude-opus-4-8")
	auxFakeTaskRoute(t, auxTaskRoute{Provider: "auto"})
	prev := auxResolveCustomRuntimeFn
	t.Cleanup(func() { auxResolveCustomRuntimeFn = prev })
	auxResolveCustomRuntimeFn = func(context.Context) (string, any, string, error) {
		return "https://configured.example/v1", "sk-configured", "chat_completions", nil
	}
	mock := &auxCallFakeClient{base: "https://configured.example/v1", key: "sk-configured"}
	calls := auxRecordResolve(t, mock, func(string) string { return "claude-opus-4-8" })
	_, client, _, err := ResolveVisionProviderClient(ctx, "", "", "", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if client != AuxClient(mock) {
		t.Fatalf("client %v", client)
	}
	last := (*calls)[len(*calls)-1]
	if last.Opts.ExplicitBaseURL != "https://configured.example/v1" || last.Opts.ExplicitAPIKey != "sk-configured" {
		t.Fatalf("call %+v", last)
	}
}
