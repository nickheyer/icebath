package provider

import (
	"context"
	"reflect"
	"testing"
)

// Agent of tests/agent/test_unset_reasoning_effort_default.py _Agent on the relay custom route
func unsetEffortAgent(cfg map[string]any, providerName, apiMode string) *fakeChatAgent {
	return &fakeChatAgent{reasoning: cfg, provider: providerName, model: "moonshotai/kimi-k3", apiMode: apiMode, baseURL: "http://relay.example/v1"}
}

// reasoning_effort the custom profile puts on the wire for a reasoning config
func customWireField(t *testing.T, ctx context.Context, cfg map[string]any) any {
	t.Helper()
	profile, ok := GetProviderProfile(ctx, "custom:relay")
	if !ok {
		t.Fatal("no custom:relay profile")
	}
	var reasoning any
	if cfg != nil {
		reasoning = cfg
	}
	kwargs, err := GetTransport("chat_completions").BuildKwargs("moonshotai/kimi-k3", []map[string]any{{"role": "user", "content": "hi"}}, nil,
		map[string]any{"provider_profile": &profile, "reasoning_config": reasoning, "base_url": "http://relay.example/v1"})
	if err != nil {
		t.Fatal(err)
	}
	return kwargs["reasoning_effort"]
}

func TestUnsetEffortGoesOutAsMediumAndExplicitEffortsAreUntouched(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	cases := []struct {
		configured map[string]any
		wire       string
	}{
		{nil, "medium"},
		{map[string]any{"enabled": true, "effort": "low"}, "low"},
		{map[string]any{"enabled": false}, "none"},
	}
	for _, c := range cases {
		agent := unsetEffortAgent(c.configured, "custom:relay", "chat_completions")
		sent := ReasoningConfigForWire(ctx, agent)
		if got := customWireField(t, ctx, sent); got != c.wire {
			t.Fatalf("configured %v went out as %v want %q", c.configured, got, c.wire)
		}
		if !reflect.DeepEqual(agent.wire, sent) {
			t.Fatalf("wire record %v sent %v", agent.wire, sent)
		}
	}
}

func TestUnsetEffortDefaultKeepsTheFieldOffWhereItWouldBeWrong(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	prev := modelsDevCapabilitiesFn
	modelsDevCapabilitiesFn = func(context.Context, string, string, bool) (ModelCapabilities, bool) {
		off := false
		return ModelCapabilities{SupportsReasoning: &off}, true
	}
	noReasoning := ReasoningConfigForWire(ctx, unsetEffortAgent(nil, "custom:relay", "chat_completions"))
	modelsDevCapabilitiesFn = prev
	if noReasoning != nil {
		t.Fatalf("model without reasoning support got %v", noReasoning)
	}
	rejected := unsetEffortAgent(nil, "custom:relay", "chat_completions")
	rejected.effortRej = true
	if got := ReasoningConfigForWire(ctx, rejected); got != nil {
		t.Fatalf("rejected effort got %v", got)
	}
	ollama := unsetEffortAgent(nil, "custom:relay", "chat_completions")
	ollama.ollamaCtx, ollama.ollamaThinks = 8192, false
	if got := ReasoningConfigForWire(ctx, ollama); got != nil {
		t.Fatalf("ollama without thinking got %v", got)
	}
	if got := ReasoningConfigForWire(ctx, unsetEffortAgent(nil, "nous", "chat_completions")); got != nil {
		t.Fatalf("nous got %v", got)
	}
	if got := UnsetReasoningDefault(ctx, unsetEffortAgent(nil, "custom:relay", "anthropic_messages")); got != nil {
		t.Fatalf("anthropic_messages got %v", got)
	}
}
