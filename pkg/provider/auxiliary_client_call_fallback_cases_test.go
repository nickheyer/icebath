package provider

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// Fixes the task route the call path resolves, as hermes tests patch _resolve_task_provider_model
func auxFixedTaskRoute(t *testing.T, route auxTaskRoute) {
	t.Helper()
	prev := auxResolveTaskProviderModelFn
	auxResolveTaskProviderModelFn = func(context.Context, any, string, string, string, any) auxTaskRoute { return route }
	t.Cleanup(func() { auxResolveTaskProviderModelFn = prev })
}

// Fake client answering each call from the scripted outcomes in order, the last one repeating
func auxScriptedCallClient(base, key string, outcomes ...any) *auxCallFakeClient {
	c := &auxCallFakeClient{base: base, key: key}
	c.create = func(context.Context, map[string]any) (any, error) {
		c.mu.Lock()
		idx := len(c.sent) - 1
		c.mu.Unlock()
		if idx >= len(outcomes) {
			idx = len(outcomes) - 1
		}
		if err, ok := outcomes[idx].(error); ok {
			return nil, err
		}
		return outcomes[idx], nil
	}
	return c
}

// Fallback chain seams answering nothing, the payment seam left for the test to script
func auxNoConfiguredChains(t *testing.T) {
	t.Helper()
	auxSwapFallbackSeams(t)
	none := func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return nil, "", "", nil
	}
	auxTryConfiguredFallbackChainFn, auxTryMainFallbackChainFn = none, none
}

// Expired bearer error the way the hermes fixture class _AuxAuth401 raises it
func auxAuth401(message string) error {
	return &AuxSDKError{Class: "_AuxAuth401", Message: message, StatusCode: 401}
}

func auxCallCount(c *auxCallFakeClient) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

func TestCallLlmPaymentFallbackStatuslessStructuredErrorAfterParamStripTriggersConfiguredFallback(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	auxSwapFallbackSeams(t)
	primary := auxScriptedCallClient("https://relay.example/v1", "test-key",
		auxExc("Unsupported parameter: temperature", 0), auxSDKStreamError(map[string]any{"type": "server_error", "code": "overloaded"}))
	fallback := auxScriptedCallClient("https://openrouter.ai/api/v1", "or", auxCallOK("fallback response"))
	auxSwapCachedClient(t, primary, "virtual-model")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "custom", Model: "virtual-model", BaseURL: "https://relay.example/v1", APIKey: "test-key"})
	reason := ""
	auxTryConfiguredFallbackChainFn = func(_ context.Context, _, _, why, _, _ string, _ FailureScope) (AuxClient, string, string, error) {
		reason = why
		return fallback, "fallback-model", "fallback_chain[0](openrouter)", nil
	}
	auxTryMainAgentModelFallbackFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return nil, "", "", nil
	}
	if _, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": []any{map[string]any{"role": "user", "content": "summarize"}}, "temperature": 0.3}); err != nil {
		t.Fatal(err)
	}
	if auxCallCount(fallback) == 0 || auxCallCount(primary) != 2 {
		t.Fatalf("fallback calls %d primary calls %d", auxCallCount(fallback), auxCallCount(primary))
	}
	if _, has := primary.sent[1]["temperature"]; has || reason != "structured provider error" {
		t.Fatalf("retry kwargs %v reason %q", primary.sent[1], reason)
	}
}

func TestCallLlmPaymentFallback429RateLimitTriggersFallback(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	auxNoConfiguredChains(t)
	primary := auxScriptedCallClient("https://api.example/v1", "k", auxExc("Rate limit exceeded, try again in 60 seconds", 429))
	fallback := auxScriptedCallClient("https://openrouter.ai/api/v1", "or", auxCallOK("fallback response"))
	auxSwapCachedClient(t, primary, "xiaomi/mimo-v2-pro")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "auto", Model: "xiaomi/mimo-v2-pro"})
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		return fallback, "fallback-model", "openrouter", nil
	}
	if _, err := CallLLM(ctx, map[string]any{"task": "session_search", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}); err != nil {
		t.Fatal(err)
	}
	if auxCallCount(fallback) == 0 {
		t.Fatal("fallback client not used")
	}
}

func TestCallLlmPaymentFallback401AuthErrorTriggersFallbackInAutoMode(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	auxNoConfiguredChains(t)
	primary := auxScriptedCallClient("https://api.minimax.chat/v1", "k", auxAuth401("expired key"))
	fallback := auxScriptedCallClient("https://openrouter.ai/api/v1", "or", auxCallOK("fallback auth response"))
	auxSwapCachedClient(t, primary, "minimax/minimax-m2.7")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "auto", Model: "minimax/minimax-m2.7"})
	reason := ""
	auxTryPaymentFallbackFn = func(_ context.Context, _, _, why, _ string, _ FailureScope, _ map[string]any) (AuxClient, string, string, error) {
		reason = why
		return fallback, "fallback-model", "openrouter", nil
	}
	result, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if err != nil || auxActualContent(t, result) != "fallback auth response" {
		t.Fatalf("result %v err %v", result, err)
	}
	if auxCallCount(fallback) == 0 || reason != "auth error" {
		t.Fatalf("fallback calls %d reason %q", auxCallCount(fallback), reason)
	}
}

// Stream deadline error whose type name carries Timeout, like the real Codex stream deadline error
func auxStreamTimeoutErr() error {
	return &ReadTimeout{msg: "Codex auxiliary Responses stream exceeded 120.0s total timeout"}
}

func TestStaleFallbackCandidateSkipStaleAnthropicFallbackRefreshesAndRetries(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	auxNoConfiguredChains(t)
	primary := auxScriptedCallClient("https://chatgpt.com/backend-api/codex", "codex", auxStreamTimeoutErr())
	stale := auxScriptedCallClient("https://api.anthropic.com", "expired-anthropic-token", auxAuth401("Invalid bearer token"))
	fresh := auxScriptedCallClient("https://api.anthropic.com", "fresh", auxCallOK("fresh-fallback"))
	prevCached := auxGetCachedClientFn
	auxGetCachedClientFn = func(_ context.Context, req auxCachedClientRequest) (AuxClient, string, error) {
		if req.Provider == "anthropic" {
			return fresh, "claude-haiku-4-5-20251001", nil
		}
		return primary, "gpt-5.5", nil
	}
	t.Cleanup(func() { auxGetCachedClientFn = prevCached })
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "auto"})
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		return stale, "claude-haiku-4-5-20251001", "anthropic", nil
	}
	var refreshed [][2]string
	prevRefresh := auxRefreshProviderCredentialsFn
	auxRefreshProviderCredentialsFn = func(_ context.Context, provider, failedAPIKey string) bool {
		refreshed = append(refreshed, [2]string{provider, failedAPIKey})
		return true
	}
	t.Cleanup(func() { auxRefreshProviderCredentialsFn = prevRefresh })
	result, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": []any{map[string]any{"role": "user", "content": "summarize"}}})
	if err != nil || auxActualContent(t, result) != "fresh-fallback" {
		t.Fatalf("result %v err %v", result, err)
	}
	if len(refreshed) != 1 || refreshed[0] != [2]string{"anthropic", "expired-anthropic-token"} {
		t.Fatalf("refresh calls %v", refreshed)
	}
	if auxCallCount(stale) != 1 || auxCallCount(fresh) != 1 {
		t.Fatalf("stale calls %d fresh calls %d", auxCallCount(stale), auxCallCount(fresh))
	}
}

func TestStaleFallbackCandidateSkipUnrefreshableStaleCandidateIsSkippedToNext(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	auxNoConfiguredChains(t)
	primary := auxScriptedCallClient("https://chatgpt.com/backend-api/codex", "codex", auxStreamTimeoutErr())
	stale := auxScriptedCallClient("https://api.anthropic.com", "expired", auxAuth401("Invalid bearer token"))
	healthy := auxScriptedCallClient("https://openrouter.ai/api/v1", "or", auxCallOK("openrouter-serves"))
	auxSwapCachedClient(t, primary, "gpt-5.5")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "auto"})
	walks := 0
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		walks++
		if walks == 1 {
			return stale, "claude-haiku-4-5-20251001", "anthropic", nil
		}
		return healthy, "fallback-model", "openrouter", nil
	}
	prevRefresh := auxRefreshProviderCredentialsFn
	auxRefreshProviderCredentialsFn = func(context.Context, string, string) bool { return false }
	t.Cleanup(func() { auxRefreshProviderCredentialsFn = prevRefresh })
	logs := auxCaptureLogs(t)
	result, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": []any{map[string]any{"role": "user", "content": "summarize"}}})
	if err != nil || auxActualContent(t, result) != "openrouter-serves" {
		t.Fatalf("result %v err %v", result, err)
	}
	if walks != 2 || auxCallCount(stale) != 1 || auxCallCount(healthy) != 1 {
		t.Fatalf("walks %d stale calls %d healthy calls %d", walks, auxCallCount(stale), auxCallCount(healthy))
	}
	if !auxIsProviderUnhealthy(ctx, "anthropic", "https://api.anthropic.com") || auxIsProviderUnhealthy(ctx, "openrouter", "") {
		t.Fatal("stale anthropic candidate not quarantined alone")
	}
	marks := logs.matching("stale fallback credential")
	if len(marks) != 1 || !strings.Contains(marks[0].msg, "anthropic") {
		t.Fatalf("quarantine marks %v", marks)
	}
}

func TestStaleFallbackCandidateSkipNonAuthFallbackErrorStillRaises(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	auxNoConfiguredChains(t)
	primary := auxScriptedCallClient("https://chatgpt.com/backend-api/codex", "codex", auxStreamTimeoutErr())
	broken := auxScriptedCallClient("https://api.anthropic.com", "k", errors.New("malformed response"))
	auxSwapCachedClient(t, primary, "gpt-5.5")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "auto"})
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		return broken, "claude-haiku-4-5-20251001", "anthropic", nil
	}
	_, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": []any{map[string]any{"role": "user", "content": "summarize"}}})
	if err == nil || !strings.Contains(err.Error(), "malformed response") {
		t.Fatalf("err %v", err)
	}
}

func TestAuxiliaryFallbackLayeringExplicitProviderRateLimitTriggersFallback(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	auxSwapFallbackSeams(t)
	primary := auxScriptedCallClient("https://chatgpt.com/backend-api/codex", "codex", auxExc("Rate limit exceeded, try again in 60 seconds", 429))
	fallback := auxScriptedCallClient("https://opencode.example/v1", "oc", auxCallOK("from fallback chain"))
	auxSwapCachedClient(t, primary, "gpt-5.5")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "openai-codex", Model: "gpt-5.5"})
	chainCalls := 0
	auxTryConfiguredFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		chainCalls++
		return fallback, "deepseek-v4-pro", "fallback_chain[0](opencode-go)", nil
	}
	auxTryMainAgentModelFallbackFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		t.Error("main agent fallback must not run when the chain succeeds")
		return nil, "", "", nil
	}
	if _, err := CallLLM(ctx, map[string]any{"task": "kanban_decomposer", "messages": []any{map[string]any{"role": "user", "content": "decompose this"}}}); err != nil {
		t.Fatal(err)
	}
	if chainCalls == 0 || auxCallCount(fallback) == 0 {
		t.Fatalf("chain calls %d fallback calls %d", chainCalls, auxCallCount(fallback))
	}
}

func TestAuxiliaryFallbackLayeringWarningEmittedWhenAllFallbacksExhausted(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	auxSwapFallbackSeams(t)
	primary := auxScriptedCallClient("https://api.z.ai/api/paas/v4", "k", auxExc("Payment Required: insufficient credits", 402))
	auxSwapCachedClient(t, primary, "glm-4v-flash")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "glm", Model: "glm-4v-flash"})
	none := func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return nil, "", "", nil
	}
	auxTryConfiguredFallbackChainFn, auxTryMainAgentModelFallbackFn = none, none
	logs := auxCaptureLogs(t)
	_, err := CallLLM(ctx, map[string]any{"task": "vision", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if err == nil || !strings.Contains(err.Error(), "Payment Required") {
		t.Fatalf("err %v", err)
	}
	warned := false
	for _, rec := range logs.matching("") {
		if rec.level >= slog.LevelWarn {
			warned = true
		}
	}
	if !warned {
		t.Fatal("Expected a user-visible warning before the re-raise")
	}
}

func TestAuxiliaryFallbackLayeringExplicitProviderNoClientUsesConfiguredChainBeforeError(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	auxSwapFallbackSeams(t)
	chain := auxScriptedCallClient("https://chatgpt.com/backend-api/codex", "codex", auxCallOK("from configured chain"))
	auxSwapCachedClient(t, nil, "")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "ollama-cloud", Model: "deepseek-v4-flash:cloud"})
	var calls [][3]string
	auxTryConfiguredFallbackChainFn = func(_ context.Context, task, failed, reason, _, _ string, _ FailureScope) (AuxClient, string, string, error) {
		calls = append(calls, [3]string{task, failed, reason})
		return chain, "gpt-5.4-mini", "fallback_chain[0](openai-codex)", nil
	}
	result, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if err != nil || auxActualContent(t, result) != "from configured chain" || auxCallCount(chain) == 0 {
		t.Fatalf("result %v err %v", result, err)
	}
	if len(calls) != 1 || calls[0] != [3]string{"compression", "ollama-cloud", "provider unavailable"} {
		t.Fatalf("chain calls %v", calls)
	}
}

func TestTryMainAgentModelFallbackReturnsNoneWhenMainProviderIsAuto(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	prevProvider, prevModel := auxReadMainProvider, auxReadMainModel
	auxReadMainProvider = func(context.Context) string { return "auto" }
	auxReadMainModel = func(context.Context) string { return "some-model" }
	t.Cleanup(func() { auxReadMainProvider, auxReadMainModel = prevProvider, prevModel })
	client, model, label, err := auxTryMainAgentModelFallback(ctx, "glm", "vision", "", "", "", "")
	if err != nil || client != nil || model != "" || label != "" {
		t.Fatalf("got %v %q %q %v", client, model, label, err)
	}
}

func TestTryMainAgentModelFallbackResolvesMainProviderClient(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	prevProvider, prevModel := auxReadMainProvider, auxReadMainModel
	auxReadMainProvider = func(context.Context) string { return "openrouter" }
	auxReadMainModel = func(context.Context) string { return "anthropic/claude-sonnet-4" }
	t.Cleanup(func() { auxReadMainProvider, auxReadMainModel = prevProvider, prevModel })
	fake := &auxCallFakeClient{}
	auxFakeResolveProviderClient(t, func(string, string, ProviderClientOptions) (AuxClient, string) {
		return fake, "anthropic/claude-sonnet-4"
	})
	client, model, label, err := auxTryMainAgentModelFallback(ctx, "glm", "vision", "", "", "", "")
	if err != nil || client != AuxClient(fake) || model != "anthropic/claude-sonnet-4" || label != "main-agent(openrouter)" {
		t.Fatalf("got %v %q %q %v", client, model, label, err)
	}
}
