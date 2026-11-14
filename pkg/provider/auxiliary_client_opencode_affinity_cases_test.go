package provider

import (
	"context"
	"testing"
)

// Header value from the extra_headers of built kwargs, false when the header is absent
func auxKwargsHeader(kwargs map[string]any, name string) (string, bool) {
	switch h := kwargs["extra_headers"].(type) {
	case map[string]string:
		v, ok := h[name]
		return v, ok
	case map[string]any:
		v, ok := h[name]
		s, _ := v.(string)
		return s, ok
	}
	return "", false
}

func auxOpencodeRuntime() map[string]any {
	return map[string]any{"provider": "opencode-zen", "model": "glm-5", "base_url": "https://opencode.ai/zen/v1",
		"api_key": "test-key", "session_id": "sess-affinity-1"}
}

// Routes every task to a scripted opencode zen client recording its kwargs
func auxRouteToFakeOpencodeClient(t *testing.T) (context.Context, *auxScriptedClient) {
	t.Helper()
	ctx := auxMainTestHome(t, "")
	prev := auxResolveTaskProviderModelFn
	t.Cleanup(func() { auxResolveTaskProviderModelFn = prev })
	auxResolveTaskProviderModelFn = func(context.Context, any, string, string, string, any) auxTaskRoute {
		return auxTaskRoute{Provider: "opencode-zen", Model: "glm-5", BaseURL: "https://opencode.ai/zen/v1", APIKey: "test-key"}
	}
	client := &auxScriptedClient{base: "https://opencode.ai/zen/v1", create: func(map[string]any) (any, error) {
		return auxAssistantResponse("glm-5", "ok"), nil
	}}
	auxSwapCachedClient(t, client, "glm-5")
	return ctx, client
}

func TestAuxiliaryCallsShareTheMainTurnSessionKey(t *testing.T) {
	ctx := SetRuntimeMain(auxMainTestHome(t, ""), "opencode-go", "glm-5", RuntimeMainOptions{BaseURL: "https://opencode.ai/zen/go/v1", SessionID: "sess-affinity-1"})
	kwargs, err := auxBuildCallKwargs(ctx, "opencode-go", "glm-5", auxConcurrencyMessages("hi"), auxCallKwargsOptions{BaseURL: "https://opencode.ai/zen/go/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := auxKwargsHeader(kwargs, "x-opencode-session"); !ok || v != "sess-affinity-1" {
		t.Fatalf("session header %q %v in %v", v, ok, kwargs["extra_headers"])
	}
	other, err := auxBuildCallKwargs(ctx, "openrouter", "x", auxConcurrencyMessages("hi"), auxCallKwargsOptions{BaseURL: "https://openrouter.ai/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := auxKwargsHeader(other, "x-opencode-session"); ok {
		t.Fatalf("openrouter carried the opencode session %v", other["extra_headers"])
	}
}

func auxOutOfTurnCase(t *testing.T, asyncMode bool, task string) {
	t.Helper()
	ctx, client := auxRouteToFakeOpencodeClient(t)
	call := CallLLM
	if asyncMode {
		call = AsyncCallLLM
	}
	if _, err := call(ctx, map[string]any{"task": task, "main_runtime": auxOpencodeRuntime(), "messages": auxConcurrencyMessages("hi")}); err != nil {
		t.Fatal(err)
	}
	if n := client.callCount(); n != 1 {
		t.Fatalf("create called %d times", n)
	}
	if v, ok := auxKwargsHeader(client.calls[0], "x-opencode-session"); !ok || v != "sess-affinity-1" {
		t.Fatalf("session header %q %v in %v", v, ok, client.calls[0]["extra_headers"])
	}
	if auxRuntimeMain(ctx) != nil {
		t.Fatal("the explicit binding must not leak past the call")
	}
}

func TestSyncOutOfTurnCallBindsTheExplicitMainRuntimeSession(t *testing.T) {
	auxOutOfTurnCase(t, false, "title_generation")
}

func TestAsyncOutOfTurnCallBindsTheExplicitMainRuntimeSession(t *testing.T) {
	auxOutOfTurnCase(t, true, "approval")
}

func TestStatelessOneshotStillSendsAnOpencodeSessionHeader(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kwargs, err := auxBuildCallKwargs(ctx, "opencode-go", "glm-5", auxConcurrencyMessages("hi"), auxCallKwargsOptions{BaseURL: "https://opencode.ai/zen/go/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := auxKwargsHeader(kwargs, "x-opencode-session"); !ok || v == "" {
		t.Fatalf("stateless call lost its ephemeral session header %v", kwargs["extra_headers"])
	}
	if OpencodeSessionHeaders(ctx, "opencode-go", "", "")["x-opencode-session"] == "" {
		t.Fatal("an opencode provider without a session must carry an ephemeral key")
	}
	if got := OpencodeSessionHeaders(ctx, "openrouter", "https://openrouter.ai/api/v1", ""); len(got) != 0 {
		t.Fatalf("openrouter headers %v", got)
	}
}
