package provider

import (
	"context"
	"testing"
)

// Codex route whose cached client rejects max_tokens on every call, the validator bypassed
func auxMaxTokensRejectingFixture(t *testing.T) (context.Context, *auxCallFakeClient) {
	t.Helper()
	ctx := auxMainTestHome(t, "")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "openai-codex", Model: "gpt-5.5"})
	client := auxScriptedCallClient("https://api.openai.com/v1", "k", auxRuntimeError("HTTP 400: Unsupported parameter: max_tokens"))
	auxSwapCachedClient(t, client, "gpt-5.5")
	prev := auxValidateLLMResponseFn
	t.Cleanup(func() { auxValidateLLMResponseFn = prev })
	auxValidateLLMResponseFn = func(_ context.Context, response any, _, _, _ string) (any, error) { return response, nil }
	return ctx, client
}

func TestMaxTokensRetryHardeningSyncMaxTokensRetrySkippedWhenMaxTokensIsNone(t *testing.T) {
	ctx, client := auxMaxTokensRejectingFixture(t)
	temperature := 0.3
	_, err := auxCallLLM(ctx, auxCallRequest{Task: "session_search", Messages: []any{map[string]any{"role": "user", "content": "hi"}}, Temperature: &temperature})
	if err == nil {
		t.Fatal("the unsupported max_tokens error must surface")
	}
	if auxCallCount(client) != 1 {
		t.Fatalf("calls %d", auxCallCount(client))
	}
}

func TestMaxTokensRetryHardeningAsyncMaxTokensRetrySkippedWhenMaxTokensIsNone(t *testing.T) {
	ctx, client := auxMaxTokensRejectingFixture(t)
	_, err := AsyncCallLLM(ctx, map[string]any{"task": "session_search", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "temperature": 0.3})
	if err == nil {
		t.Fatal("the unsupported max_tokens error must surface")
	}
	if auxCallCount(client) != 1 {
		t.Fatalf("calls %d", auxCallCount(client))
	}
}
