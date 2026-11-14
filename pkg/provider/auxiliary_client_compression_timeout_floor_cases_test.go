package provider

import (
	"context"
	"testing"
)

const (
	auxCompressionTimeoutFloor         = 300.0
	auxCompressionConfigTimeout        = 120.0
	auxCompressionTimeoutFloorConfig   = "auxiliary:\n  compression:\n    timeout: 120\n  title_generation:\n    timeout: 30\n  session_search:\n    timeout: 30\n"
	auxCompressionTimeoutFloorLowValue = 30.0
)

// Codex route, a recording client at the OpenAI base and the validator bypassed over the config derived task timeouts
func auxTimeoutFloorFixture(t *testing.T) (context.Context, *auxScriptedClient) {
	t.Helper()
	ctx := auxMainTestHome(t, auxCompressionTimeoutFloorConfig)
	prevRoute := auxResolveTaskProviderModelFn
	t.Cleanup(func() { auxResolveTaskProviderModelFn = prevRoute })
	auxResolveTaskProviderModelFn = func(context.Context, any, string, string, string, any) auxTaskRoute {
		return auxTaskRoute{Provider: "openai-codex", Model: "gpt-5.5"}
	}
	client := &auxScriptedClient{base: "https://api.openai.com/v1", create: func(map[string]any) (any, error) { return auxDummyResponse(), nil }}
	auxSwapCachedClient(t, client, "gpt-5.5")
	prev := auxValidateLLMResponseFn
	t.Cleanup(func() { auxValidateLLMResponseFn = prev })
	auxValidateLLMResponseFn = func(_ context.Context, response any, _, _, _ string) (any, error) { return response, nil }
	return ctx, client
}

// Timeout kwarg of the last create call the client recorded
func auxLastTimeoutKwarg(t *testing.T, client *auxScriptedClient) float64 {
	t.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.calls) == 0 {
		t.Fatal("no create call reached the client")
	}
	timeout, ok := client.calls[len(client.calls)-1]["timeout"].(float64)
	if !ok {
		t.Fatalf("timeout kwarg %v", client.calls[len(client.calls)-1]["timeout"])
	}
	return timeout
}

func auxTimeoutFloorMessages(content string) []any {
	return []any{map[string]any{"role": "user", "content": content}}
}

func TestCompressionTimeoutFloorSyncConfigDerivedCompressionTimeoutIsRaisedToFloor(t *testing.T) {
	ctx, client := auxTimeoutFloorFixture(t)
	if _, err := auxCallLLM(ctx, auxCallRequest{Task: "compression", Messages: auxTimeoutFloorMessages("summarise this")}); err != nil {
		t.Fatal(err)
	}
	timeout := auxLastTimeoutKwarg(t, client)
	if timeout < auxCompressionTimeoutFloor {
		t.Fatalf("compression timeout %v should be >= floor %v", timeout, auxCompressionTimeoutFloor)
	}
	if timeout <= auxCompressionConfigTimeout {
		t.Fatal("the too-low config timeout must not pass through unchanged")
	}
}

func TestCompressionTimeoutFloorSyncNonCompressionTaskIsNotFloored(t *testing.T) {
	ctx, client := auxTimeoutFloorFixture(t)
	if _, err := auxCallLLM(ctx, auxCallRequest{Task: "title_generation", Messages: auxTimeoutFloorMessages("x")}); err != nil {
		t.Fatal(err)
	}
	if timeout := auxLastTimeoutKwarg(t, client); timeout != auxCompressionTimeoutFloorLowValue {
		t.Fatalf("non-compression task timeout must stay %v, got %v", auxCompressionTimeoutFloorLowValue, timeout)
	}
}

func TestCompressionTimeoutFloorAsyncAsyncConfigDerivedCompressionTimeoutIsRaisedToFloor(t *testing.T) {
	ctx, client := auxTimeoutFloorFixture(t)
	if _, err := AsyncCallLLM(ctx, map[string]any{"task": "compression", "messages": auxTimeoutFloorMessages("summarise this")}); err != nil {
		t.Fatal(err)
	}
	if timeout := auxLastTimeoutKwarg(t, client); timeout < auxCompressionTimeoutFloor {
		t.Fatalf("async compression timeout %v should be >= floor %v", timeout, auxCompressionTimeoutFloor)
	}
}

func TestCompressionTimeoutFloorAsyncAsyncExplicitPerCallTimeoutIsNotFloored(t *testing.T) {
	ctx, client := auxTimeoutFloorFixture(t)
	if _, err := AsyncCallLLM(ctx, map[string]any{"task": "compression", "messages": auxTimeoutFloorMessages("x"), "timeout": 45.0}); err != nil {
		t.Fatal(err)
	}
	if timeout := auxLastTimeoutKwarg(t, client); timeout != 45.0 {
		t.Fatalf("explicit timeout %v", timeout)
	}
}

func TestCompressionTimeoutFloorAsyncAsyncNonCompressionTaskIsNotFloored(t *testing.T) {
	ctx, client := auxTimeoutFloorFixture(t)
	if _, err := AsyncCallLLM(ctx, map[string]any{"task": "session_search", "messages": auxTimeoutFloorMessages("x")}); err != nil {
		t.Fatal(err)
	}
	if timeout := auxLastTimeoutKwarg(t, client); timeout != auxCompressionTimeoutFloorLowValue {
		t.Fatalf("timeout %v", timeout)
	}
}
