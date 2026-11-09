package provider

import (
	"context"
	"log/slog"
	"reflect"
	"testing"
)

func TestAuxiliaryClientPoisonedCacheEvictionCallLlmEvictsOnConnectionErrorWithExplicitProvider(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxIsolateClientCache(t)
	auxClearUnhealthy(t)
	poisoned := auxScriptedCallClient("https://chatgpt.com/backend-api/codex", "k", &AuxTransportError{Class: "ConnectionError", Message: "transport closed"})
	key := auxSeedCachedClient(t, ctx, "openai-codex", poisoned, "gpt-5.5")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "openai-codex", Model: "gpt-5.5"})
	auxSwapCachedClient(t, poisoned, "gpt-5.5")
	auxSwapFallbackSeams(t)
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		return nil, "", "", nil
	}
	prevBackoff := auxTransientRetryBackoffBase
	auxTransientRetryBackoffBase = 0
	t.Cleanup(func() { auxTransientRetryBackoffBase = prevBackoff })
	_, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxUserMessages("x")})
	if !auxIsConnectionError(err) {
		t.Fatalf("err %v", err)
	}
	if auxCacheHolds(key) {
		t.Fatal("connection error must evict cached client so the next call rebuilds")
	}
}

func TestBuildCallKwargsToolDedupDuplicateToolNamesAreDeduplicated(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	tool := func(name string) any {
		return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": "Tool " + name,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}
	}
	tools := []any{tool("lcm_grep"), tool("lcm_describe"), tool("lcm_grep"), tool("lcm_expand"), tool("lcm_describe")}
	kwargs, err := auxBuildCallKwargs(ctx, "google", "gemini-2.5-pro", []any{}, auxCallKwargsOptions{Tools: tools, Timeout: 120})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	seen := map[string]bool{}
	for _, item := range auxKwargMaps(kwargs["tools"]) {
		fn, _ := item["function"].(map[string]any)
		name := pyStr(fn["name"])
		if seen[name] {
			t.Fatalf("Duplicate tool names found: %v", kwargs["tools"])
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) != 3 {
		t.Fatalf("tools %v", names)
	}
}

// Default headers of a resolved OpenAI SDK client
func auxClientDefaultHeaders(t *testing.T, client AuxClient) map[string]string {
	t.Helper()
	sdk, ok := client.(*auxOpenAIClient)
	if !ok {
		t.Fatalf("got %T", client)
	}
	sdk.mu.Lock()
	defer sdk.mu.Unlock()
	out := map[string]string{}
	for k, v := range sdk.customHeaders {
		out[k] = v
	}
	return out
}

func TestNvidiaBillingHeadersResolveProviderClientCloudAddsBillingOriginHeader(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxIsolateClientCache(t)
	t.Setenv("NVIDIA_API_KEY", "nvidia-key")
	t.Setenv("NVIDIA_BASE_URL", "")
	client, model, err := ResolveProviderClient(ctx, "nvidia", "nvidia/test-model", ProviderClientOptions{})
	if err != nil || client == nil || model != "nvidia/test-model" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
	if got := auxClientDefaultHeaders(t, client)["X-BILLING-INVOKE-ORIGIN"]; got != auxNvidiaNIMCloudHeaders["X-BILLING-INVOKE-ORIGIN"] {
		t.Fatalf("billing origin header %q", got)
	}
}

func TestNvidiaBillingHeadersResolveProviderClientLocalNimSkipsBillingOriginHeader(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxIsolateClientCache(t)
	t.Setenv("NVIDIA_API_KEY", "nvidia-key")
	t.Setenv("NVIDIA_BASE_URL", "http://localhost:8000/v1")
	client, model, err := ResolveProviderClient(ctx, "nvidia", "nvidia/test-model", ProviderClientOptions{})
	if err != nil || client == nil || model != "nvidia/test-model" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
	if _, has := auxClientDefaultHeaders(t, client)["X-BILLING-INVOKE-ORIGIN"]; has {
		t.Fatal("local NIM got the billing origin header")
	}
}

func TestOpenRouterExplicitApiKeyResolveProviderClientPassesExplicitApiKeyToOpenrouter(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxIsolateClientCache(t)
	t.Setenv("OPENROUTER_API_KEY", "env-fallback-key")
	client, _, err := ResolveProviderClient(ctx, "openrouter", "", ProviderClientOptions{ExplicitAPIKey: "explicit-pool-key"})
	if err != nil || client == nil {
		t.Fatalf("got %v %v", client, err)
	}
	if client.APIKey() != "explicit-pool-key" {
		t.Fatalf("Expected explicit_api_key to be passed, got: %q", client.APIKey())
	}
}

func TestAnthropicExplicitApiKeyResolveProviderClientPassesExplicitApiKeyToAnthropic(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	captured := auxFakeAnthropicBuild(t, func(context.Context, string) (bool, *PooledCredential) { return false, nil }, "env-key")
	client, _, err := ResolveProviderClient(ctx, "anthropic", "", ProviderClientOptions{ExplicitAPIKey: "explicit-fallback-key"})
	if err != nil || client == nil {
		t.Fatalf("got %v %v", client, err)
	}
	if captured[0] != "explicit-fallback-key" {
		t.Fatalf("resolve_provider_client must forward explicit_api_key to _try_anthropic(), got %v", captured[0])
	}
}

func TestAuxUnhealthyCacheCustomHealthUrlIdentityPreservesPathAndQueryCase(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	auxMarkProviderUnhealthy(ctx, "custom", 60, "https://Example.test/API/v1/", "", slog.LevelDebug)
	if !auxIsProviderUnhealthy(ctx, "custom", "https://example.TEST/API/v1") {
		t.Fatal("host case folded identity missed")
	}
	if auxIsProviderUnhealthy(ctx, "custom", "https://example.test/api/v1") {
		t.Fatal("path case was folded")
	}
	auxMarkProviderUnhealthy(ctx, "custom", 60, "https://example.test/API/v1?token=AbC", "", slog.LevelDebug)
	if auxIsProviderUnhealthy(ctx, "custom", "https://example.test/API/v1?token=abc") {
		t.Fatal("query case was folded")
	}
}

func TestAuxUnhealthyCacheCallLlmMarksProviderUnhealthyOn402(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	auxNoConfiguredChains(t)
	primary := auxScriptedCallClient("https://openrouter.ai/api/v1/", "k", auxExc("Payment Required: insufficient credits", 402))
	nous := auxScriptedCallClient("https://inference-api.nousresearch.com/v1", "n", auxCallOK("ok"))
	auxSwapCachedClient(t, primary, "google/gemini-3-flash-preview")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "auto", Model: "google/gemini-3-flash-preview"})
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		return nous, "n-model", "nous", nil
	}
	if auxIsProviderUnhealthy(ctx, "openrouter", "") {
		t.Fatal("openrouter unhealthy before the call")
	}
	if _, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxUserMessages("hi")}); err != nil {
		t.Fatal(err)
	}
	if !auxIsProviderUnhealthy(ctx, "openrouter", "") {
		t.Fatal("openrouter not marked unhealthy after the 402")
	}
}

func TestAuxUnhealthyCacheCustomBillingFailureKeepsDistinctEndpointEligible(t *testing.T) {
	hostedURL, localURL := "https://hosted.example/v1", "http://127.0.0.1:8080/v1"
	ctx := auxMainTestHome(t, "auxiliary:\n  compression:\n    fallback_chain:\n      - provider: custom\n        model: local-model\n        base_url: "+localURL+"\n        api_key: local\n")
	auxClearUnhealthy(t)
	hosted := auxScriptedCallClient(hostedURL, "hosted", auxExc("Payment Required: weekly usage limit", 402))
	local := auxScriptedCallClient(localURL, "local", auxCallOK("local-ok"))
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "custom", Model: "hosted-model", BaseURL: hostedURL, APIKey: "hosted"})
	auxSwapCachedClient(t, hosted, "hosted-model")
	prevEntry := auxResolveFallbackEntryFn
	auxResolveFallbackEntryFn = func(context.Context, map[string]any) (AuxClient, string, error) { return local, "local-model", nil }
	t.Cleanup(func() { auxResolveFallbackEntryFn = prevEntry })
	response, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxUserMessages("summarize")})
	if err != nil || auxActualContent(t, response) != "local-ok" {
		t.Fatalf("response %v err %v", response, err)
	}
	if !auxIsProviderUnhealthy(ctx, "custom", hostedURL) || auxIsProviderUnhealthy(ctx, "custom", localURL) {
		t.Fatalf("hosted unhealthy %v local unhealthy %v", auxIsProviderUnhealthy(ctx, "custom", hostedURL), auxIsProviderUnhealthy(ctx, "custom", localURL))
	}
	if auxCallCount(hosted) != 1 || auxCallCount(local) != 1 {
		t.Fatalf("hosted calls %d local calls %d", auxCallCount(hosted), auxCallCount(local))
	}
}

func TestAuxUnhealthyCacheCustomFallbackAuthFailureQuarantinesFailedEndpoint(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	hostedURL, localURL := "https://hosted.example/v1", "http://127.0.0.1:8080/v1"
	hosted := auxScriptedCallClient(hostedURL, "hosted", auxAuth401("expired hosted key"))
	prevCustom, prevRefresh := auxResolveCustomRuntimeFn, auxRefreshProviderCredentialsFn
	auxResolveCustomRuntimeFn = func(context.Context) (string, string, string, error) { return localURL, "", "", nil }
	auxRefreshProviderCredentialsFn = func(context.Context, string, string) bool { return false }
	t.Cleanup(func() { auxResolveCustomRuntimeFn, auxRefreshProviderCredentialsFn = prevCustom, prevRefresh })
	result, err := auxCallFallbackCandidate(ctx, hosted, "hosted-model", "fallback_chain[0](custom)", auxFallbackRequest{
		Task: "session_search", Messages: []map[string]any{{"role": "user", "content": "search"}}, EffectiveTimeout: 30, EffectiveExtraBody: map[string]any{}}, false)
	if err != nil || result != nil {
		t.Fatalf("result %v err %v", result, err)
	}
	if !auxIsProviderUnhealthy(ctx, "custom", hostedURL) || auxIsProviderUnhealthy(ctx, "custom", localURL) {
		t.Fatalf("hosted unhealthy %v local unhealthy %v", auxIsProviderUnhealthy(ctx, "custom", hostedURL), auxIsProviderUnhealthy(ctx, "custom", localURL))
	}
}

// Fixes the active custom endpoint the max tokens rule reads
func auxFixedCustomBase(t *testing.T, base string) {
	t.Helper()
	prev := auxResolveCustomRuntimeFn
	auxResolveCustomRuntimeFn = func(context.Context) (string, string, string, error) { return base, "", "", nil }
	t.Cleanup(func() { auxResolveCustomRuntimeFn = prev })
}

func TestAuxiliaryMaxTokensParamOpenrouterApiKeyPresentKeepsMaxTokensWithoutModelHint(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-test")
	auxFixedCustomBase(t, "https://openrouter.ai/api/v1")
	if got := AuxiliaryMaxTokensParam(ctx, 4096, ""); !reflect.DeepEqual(got, map[string]any{"max_tokens": 4096}) {
		t.Fatalf("got %v", got)
	}
}

func TestAuxiliaryMaxTokensParamEmptyModelFallsBackToUrlOnly(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENROUTER_API_KEY", "")
	auxFixedCustomBase(t, "https://my-gateway.example.com/v1")
	if got := AuxiliaryMaxTokensParam(ctx, 4096, ""); !reflect.DeepEqual(got, map[string]any{"max_tokens": 4096}) {
		t.Fatalf("got %v", got)
	}
}
