package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Route, cached client and identity validation, as the hermes _patches fixture installs them
func auxTransientPatches(t *testing.T, client AuxClient) context.Context {
	t.Helper()
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "openrouter", Model: "some-model"})
	auxSwapCachedClient(t, client, "some-model")
	prev := auxValidateLLMResponseFn
	auxValidateLLMResponseFn = func(_ context.Context, resp any, _, _, _ string) (any, error) { return resp, nil }
	t.Cleanup(func() { auxValidateLLMResponseFn = prev })
	return ctx
}

// SDK timeout the way a class renamed APITimeoutError raises it
func auxAPITimeout() error {
	return &AuxSDKError{Class: "APITimeoutError", Message: "Request timed out."}
}

// Fixes the vision resolver to hand back one client for every provider
func auxFixedVisionClient(t *testing.T, client AuxClient) {
	t.Helper()
	prev := auxResolveVisionProviderClientFn
	auxResolveVisionProviderClientFn = func(context.Context, string, string, string, any, bool, map[string]any) (string, AuxClient, string, error) {
		return "openrouter", client, "some-model", nil
	}
	t.Cleanup(func() { auxResolveVisionProviderClientFn = prev })
}

func auxUserMessages(text string) []any {
	return []any{map[string]any{"role": "user", "content": text}}
}

func TestTransientTransportRetryDoesNotRetryNonTransient400(t *testing.T) {
	client := auxScriptedCallClient("https://openrouter.ai/api/v1", "k", &AuxSDKError{Class: "_Err400", Message: "bad request", StatusCode: 400})
	ctx := auxTransientPatches(t, client)
	_, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxUserMessages("hi")})
	if err == nil || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("err %v", err)
	}
	if auxCallCount(client) != 1 {
		t.Fatalf("calls %d", auxCallCount(client))
	}
}

func TestTransientTransportRetryCompressionSkipsSameProviderRetryOnTimeout(t *testing.T) {
	primary := auxScriptedCallClient("https://openrouter.ai/api/v1", "k", auxAPITimeout())
	fb := auxScriptedCallClient("https://api.openai.com/v1", "k", map[string]any{"fallback": true})
	ctx := auxTransientPatches(t, primary)
	auxNoConfiguredChains(t)
	auxTryMainAgentModelFallbackFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return fb, "fb-model", "openai", nil
	}
	result, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxUserMessages("hi")})
	if err != nil || !reflect.DeepEqual(result, map[string]any{"fallback": true}) {
		t.Fatalf("result %v err %v", result, err)
	}
	if auxCallCount(primary) != 1 || auxCallCount(fb) != 1 {
		t.Fatalf("primary calls %d fallback calls %d", auxCallCount(primary), auxCallCount(fb))
	}
}

func TestTransientTransportRetryVisionSkipsSameProviderRetryOnTimeout(t *testing.T) {
	primary := auxScriptedCallClient("https://openrouter.ai/api/v1", "k", auxAPITimeout())
	fb := auxScriptedCallClient("https://api.openai.com/v1", "k", map[string]any{"fallback": true})
	ctx := auxTransientPatches(t, primary)
	auxFixedVisionClient(t, primary)
	auxNoConfiguredChains(t)
	auxTryMainAgentModelFallbackFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return fb, "fb-model", "openai", nil
	}
	result, err := CallLLM(ctx, map[string]any{"task": "vision", "messages": auxUserMessages("hi")})
	if err != nil || !reflect.DeepEqual(result, map[string]any{"fallback": true}) {
		t.Fatalf("result %v err %v", result, err)
	}
	if auxCallCount(primary) != 1 || auxCallCount(fb) != 1 {
		t.Fatalf("primary calls %d fallback calls %d", auxCallCount(primary), auxCallCount(fb))
	}
}

func TestTransientTransportRetryVisionSkipsSameProviderRetryOnTimeoutAsync(t *testing.T) {
	primary := auxScriptedCallClient("https://openrouter.ai/api/v1", "k", auxAPITimeout())
	expected := map[string]any{"fallback": true}
	fb := auxScriptedCallClient("https://api.openai.com/v1", "k", expected)
	ctx := auxTransientPatches(t, primary)
	auxFixedVisionClient(t, primary)
	auxSwapFallbackSeams(t)
	auxTryConfiguredFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return fb, "fb-model", "configured-fallback", nil
	}
	prevAsync := auxToAsyncClientFn
	auxToAsyncClientFn = func(_ context.Context, client AuxClient, _ string, _ bool) (AuxClient, string) {
		return client, "fb-model"
	}
	t.Cleanup(func() { auxToAsyncClientFn = prevAsync })
	result, err := AsyncCallLLM(ctx, map[string]any{"task": "vision", "messages": auxUserMessages("hi")})
	if err != nil || !reflect.DeepEqual(result, expected) {
		t.Fatalf("result %v err %v", result, err)
	}
	if auxCallCount(primary) != 1 {
		t.Fatalf("primary calls %d", auxCallCount(primary))
	}
}

func TestTransientTransportRetryNoProgressTimeoutStillRetriesSameProviderAsync(t *testing.T) {
	primary := auxScriptedCallClient("https://chatgpt.com/backend-api/codex", "k",
		&AuxTimeoutError{Message: "Codex auxiliary Responses stream produced no output within 60.0s (no-progress timeout, 60.2s elapsed)"},
		map[string]any{"retried": true})
	ctx := auxTransientPatches(t, primary)
	auxFixedVisionClient(t, primary)
	result, err := AsyncCallLLM(ctx, map[string]any{"task": "vision", "messages": auxUserMessages("hi")})
	if err != nil || !reflect.DeepEqual(result, map[string]any{"retried": true}) {
		t.Fatalf("result %v err %v", result, err)
	}
	if auxCallCount(primary) != 2 {
		t.Fatalf("primary calls %d", auxCallCount(primary))
	}
}

func TestTransientTransportRetryNonCriticalTaskStillRetriesSameProviderOnTimeout(t *testing.T) {
	primary := auxScriptedCallClient("https://openrouter.ai/api/v1", "k", auxAPITimeout(), map[string]any{"retried": true})
	ctx := auxTransientPatches(t, primary)
	prevBackoff := auxTransientRetryBackoffBase
	auxTransientRetryBackoffBase = 0
	t.Cleanup(func() { auxTransientRetryBackoffBase = prevBackoff })
	result, err := CallLLM(ctx, map[string]any{"task": "title", "messages": auxUserMessages("hi")})
	if err != nil || !reflect.DeepEqual(result, map[string]any{"retried": true}) {
		t.Fatalf("result %v err %v", result, err)
	}
	if auxCallCount(primary) != 2 {
		t.Fatalf("primary calls %d", auxCallCount(primary))
	}
}

func TestTransientTransportRetryTimeoutForwardsFailedModelToConfiguredChain(t *testing.T) {
	primary := auxScriptedCallClient("https://integrate.api.nvidia.com/v1", "k", auxAPITimeout())
	fb := auxScriptedCallClient("https://integrate.api.nvidia.com/v1", "k", map[string]any{"fallback": true})
	ctx := auxTransientPatches(t, primary)
	auxSwapFallbackSeams(t)
	failedModel := ""
	auxTryConfiguredFallbackChainFn = func(_ context.Context, _, _, _, model, _ string, _ FailureScope) (AuxClient, string, string, error) {
		failedModel = model
		return fb, "sibling-model", "fallback_chain[0](openrouter)", nil
	}
	auxTryMainAgentModelFallbackFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return nil, "", "", nil
	}
	result, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxUserMessages("hi")})
	if err != nil || !reflect.DeepEqual(result, map[string]any{"fallback": true}) {
		t.Fatalf("result %v err %v", result, err)
	}
	if failedModel != "some-model" {
		t.Fatalf("A timeout is model-specific — the failed model must be forwarded so a same-provider sibling can be tried, not skipped wholesale. got %q", failedModel)
	}
}

func TestAuxClientNoSdkRetriesSyncClientDisablesSdkRetries(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	client, ok := auxCreateOpenAIClient(ctx, "k", "https://x/v1", auxOpenAIOptions{}).(*auxOpenAIClient)
	if !ok {
		t.Fatalf("got %T", client)
	}
	if client.maxRetries != 0 {
		t.Fatalf("max_retries %d", client.maxRetries)
	}
}
