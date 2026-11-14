package provider

import (
	"reflect"
	"testing"
)

func TestAnthropicAuxiliaryReasoningTranslationReasoningConfigReachesNativeAnthropicWireKwargs(t *testing.T) {
	captured := auxCaptureAnthropicKwargs(t, nil, false, map[string]any{"model": "claude-fable-5", "_reasoning_config": map[string]any{"enabled": true, "effort": "medium"}})
	if !reflect.DeepEqual(captured["thinking"], map[string]any{"type": "adaptive", "display": "summarized"}) {
		t.Fatalf("thinking %v", captured["thinking"])
	}
	if !reflect.DeepEqual(captured["output_config"], map[string]any{"effort": "medium"}) {
		t.Fatalf("output_config %v", captured["output_config"])
	}
	if _, has := captured["extra_body"]; has {
		t.Fatalf("extra_body reached the wire %v", captured["extra_body"])
	}
}

func TestAnthropicAuxiliaryReasoningTranslationBuildCallKwargsPrivateReasoningOnlyForAnthropicMessages(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	rc := map[string]any{"enabled": true, "effort": "medium"}
	build := func(provider, model, base string) map[string]any {
		kwargs, err := auxBuildCallKwargs(ctx, provider, model, auxUserHi(), auxCallKwargsOptions{ReasoningConfig: rc, BaseURL: base, Timeout: 120})
		if err != nil {
			t.Fatal(err)
		}
		return kwargs
	}
	if got := build("anthropic", "claude-fable-5", "https://api.anthropic.com/v1"); !reflect.DeepEqual(got["_reasoning_config"], rc) {
		t.Fatalf("anthropic kwargs %v", got)
	}
	if got := build("custom", "claude-fable-5", "https://example.test/anthropic/v1"); !reflect.DeepEqual(got["_reasoning_config"], rc) {
		t.Fatalf("proxy kwargs %v", got)
	}
	if got := build("custom", "gpt-compatible", "https://example.test/v1"); got["_reasoning_config"] != nil {
		t.Fatalf("openai wire kwargs %v", got)
	}
}

func TestAnthropicAuxiliaryReasoningTranslationAnthropicMessagesProfileKeepsReasoningReachable(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	if _, ok := GetProviderProfile(ctx, "commandcode-anthropic"); !ok {
		t.Fatal("commandcode-anthropic profile missing")
	}
	rc := map[string]any{"enabled": false}
	kwargs, err := auxBuildCallKwargs(ctx, "commandcode-anthropic", "claude-haiku-4-5-20251001", auxUserHi(),
		auxCallKwargsOptions{ReasoningConfig: rc, BaseURL: "https://api.commandcode.ai/provider/v1", Timeout: 120})
	if err != nil || !reflect.DeepEqual(kwargs["_reasoning_config"], rc) {
		t.Fatalf("kwargs %v err %v", kwargs, err)
	}
	chat, err := auxBuildCallKwargs(ctx, "commandcode", "Qwen/Qwen3.7-Max", auxUserHi(),
		auxCallKwargsOptions{ReasoningConfig: rc, BaseURL: "https://api.commandcode.ai/provider/v1", Timeout: 120})
	if err != nil || chat["_reasoning_config"] != nil {
		t.Fatalf("chat kwargs %v err %v", chat, err)
	}
}

func TestAnthropicAuxiliaryReasoningTranslationAnthropicMessagesProfileResolvesToMessagesAdapter(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxIsolateClientCache(t)
	t.Setenv("COMMANDCODE_API_KEY", "sk-test-xxxxxxxxxxxxxxxxxxxx")
	client, _, err := ResolveProviderClient(ctx, "commandcode-anthropic", "claude-haiku-4-5-20251001", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.(*AnthropicAuxiliaryClient); !ok {
		t.Fatalf("got %T", client)
	}
}

func TestAuxiliaryProviderProfileReasoningKimiReasoningUsesTopLevelEffort(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kwargs, err := auxBuildCallKwargs(ctx, "kimi-coding", "kimi-k2-turbo-preview", auxUserHi(),
		auxCallKwargsOptions{ReasoningConfig: map[string]any{"enabled": true, "effort": "medium"}, BaseURL: "https://api.moonshot.ai/v1", Timeout: 120})
	if err != nil || kwargs["reasoning_effort"] != "high" {
		t.Fatalf("kwargs %v err %v", kwargs, err)
	}
	body, _ := kwargs["extra_body"].(map[string]any)
	if _, has := body["reasoning"]; has {
		t.Fatalf("extra_body reasoning %v", body)
	}
	if _, has := body["thinking"]; has {
		t.Fatalf("extra_body thinking %v", body)
	}
}

func TestAuxiliaryProviderProfileReasoningAsyncCallLlmPreservesProfileReasoningKwargs(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	client := auxScriptedCallClient("https://api.moonshot.ai/v1", "test-key", auxCallOK("ok"))
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "kimi-coding", Model: "kimi-k2-turbo-preview", BaseURL: "https://api.moonshot.ai/v1", APIKey: "test-key"})
	auxSwapCachedClient(t, client, "kimi-k2-turbo-preview")
	result, err := AsyncCallLLM(ctx, map[string]any{"provider": "kimi-coding", "model": "kimi-k2-turbo-preview", "messages": auxUserHi(),
		"reasoning_config": map[string]any{"enabled": true, "effort": "high"}})
	if err != nil || auxActualContent(t, result) != "ok" {
		t.Fatalf("result %v err %v", result, err)
	}
	final := client.sent[len(client.sent)-1]
	body, _ := final["extra_body"].(map[string]any)
	if _, has := body["reasoning"]; final["reasoning_effort"] != "high" || has {
		t.Fatalf("final kwargs %v", final)
	}
}
