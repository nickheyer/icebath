package provider

import (
	"context"
	"testing"
)

func TestVisionCallUsesResolvedProviderArgs(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "my-resolved-provider", Model: "my-resolved-model", BaseURL: "http://resolved", APIKey: "resolved-key", APIMode: "chat_completions"})
	fakeClient := auxScriptedCallClient("http://resolved", "resolved-key", map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "description"}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	})
	var seen [4]any
	prev := auxResolveVisionProviderClientFn
	t.Cleanup(func() { auxResolveVisionProviderClientFn = prev })
	auxResolveVisionProviderClientFn = func(_ context.Context, provider, model, baseURL string, apiKey any, _ bool, _ map[string]any) (string, AuxClient, string, error) {
		seen = [4]any{provider, model, baseURL, apiKey}
		return "my-resolved-provider", fakeClient, "my-resolved-model", nil
	}
	if _, err := auxCallLLM(ctx, auxCallRequest{Task: "vision", Provider: "raw-provider", Model: "raw-model", BaseURL: "http://raw", APIKey: "raw-key",
		Messages: []any{map[string]any{"role": "user", "content": "describe this"}}}); err != nil {
		t.Fatal(err)
	}
	if seen != [4]any{"my-resolved-provider", "my-resolved-model", "http://resolved", "resolved-key"} {
		t.Fatalf("vision resolver received the raw arguments %v", seen)
	}
}

func TestVisionBaseUrlOverrideKeepsExplicitProvider(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "zai", Model: "glm-4v", BaseURL: "https://open.bigmodel.cn/api/paas/v4", APIMode: "chat_completions"})
	fakeClient := auxBareClient{base: "https://open.bigmodel.cn/api/paas/v4", key: "k"}
	var seenProvider, seenBase string
	prev := auxResolveProviderClientFn
	t.Cleanup(func() { auxResolveProviderClientFn = prev })
	auxResolveProviderClientFn = func(_ context.Context, provider, _ string, opts ProviderClientOptions) (AuxClient, string, error) {
		seenProvider, seenBase = provider, opts.ExplicitBaseURL
		return fakeClient, "glm-4v", nil
	}
	provider, client, model, err := ResolveVisionProviderClient(ctx, "", "", "", nil, false, nil)
	if err != nil || provider != "zai" || client != fakeClient || model != "glm-4v" {
		t.Fatalf("got (%q, %v, %q, %v)", provider, client, model, err)
	}
	if seenProvider != "zai" || seenBase != "https://open.bigmodel.cn/api/paas/v4" {
		t.Fatalf("resolve_provider_client received (%q, %q)", seenProvider, seenBase)
	}
}
