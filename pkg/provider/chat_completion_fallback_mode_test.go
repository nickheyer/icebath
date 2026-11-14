package provider

import "testing"

type fakeFallbackModeAgent struct {
	azure, direct, responses bool
	asked                    [][2]string
}

func (a *fakeFallbackModeAgent) IsAzureOpenAIURL(string) bool  { return a.azure }
func (a *fakeFallbackModeAgent) IsDirectOpenAIURL(string) bool { return a.direct }
func (a *fakeFallbackModeAgent) ProviderModelRequiresResponsesAPI(model, providerName string) bool {
	a.asked = append(a.asked, [2]string{model, providerName})
	return a.responses
}

func TestFallbackAPIModeResolvedWalksProviderHostAndModelInHermesOrder(t *testing.T) {
	ctx := auxMainTestHome(t, "nous:\n  anthropic_wire: native\n")
	cases := []struct {
		name, provider, model, base, want string
		agent                             fakeFallbackModeAgent
	}{
		{"codex provider", "openai-codex", "gpt-5.6", "https://chatgpt.com/backend-api/codex", "codex_responses", fakeFallbackModeAgent{}},
		{"opencode family by host", "custom", "claude-sonnet-4-6", "https://opencode.ai/zen/v1", string(OpenCodeModelMode("opencode-zen", "claude-sonnet-4-6")), fakeFallbackModeAgent{}},
		{"nous anthropic slug on the native wire", "nous", "anthropic/claude-opus-4.6", "https://inference-api.nousresearch.com/v1", "anthropic_messages", fakeFallbackModeAgent{}},
		{"nous other slug", "nous-portal", "hermes-4-405b", "", "chat_completions", fakeFallbackModeAgent{responses: true}},
		{"anthropic wire host", "cron-anthropic", "claude-opus-4-6", "https://proxy.example.com/anthropic", "anthropic_messages", fakeFallbackModeAgent{direct: true}},
		{"azure stays on chat", "azure", "gpt-5.6", "https://x.openai.azure.com/openai/v1", "chat_completions", fakeFallbackModeAgent{azure: true, direct: true}},
		{"direct openai", "openai", "gpt-4.1", "https://api.openai.com/v1", "codex_responses", fakeFallbackModeAgent{direct: true}},
		{"model requires responses", "acme-http", "gpt-5.6", "https://proxy.example.invalid/v1", "codex_responses", fakeFallbackModeAgent{responses: true}},
		{"bedrock provider", "bedrock", "us.amazon.nova-pro-v1:0", "", "bedrock_converse", fakeFallbackModeAgent{}},
		{"bedrock runtime host", "custom", "m", "https://bedrock-runtime.us-east-1.amazonaws.com", "bedrock_converse", fakeFallbackModeAgent{}},
		{"lookalike bedrock host", "custom", "m", "https://bedrock-runtime.evil.example.com", "chat_completions", fakeFallbackModeAgent{}},
		{"plain chat", "copilot-acp", "gpt-5.6", "https://proxy.example.invalid/v1", "chat_completions", fakeFallbackModeAgent{}},
	}
	for _, c := range cases {
		agent := c.agent
		got, err := FallbackAPIModeResolved(ctx, &agent, c.provider, c.model, c.base)
		if err != nil || got != c.want {
			t.Fatalf("%s: got %q %v want %q", c.name, got, err, c.want)
		}
	}
	agent := &fakeFallbackModeAgent{}
	if _, err := FallbackAPIModeResolved(ctx, agent, "acme-http", "gpt-5.6", "https://proxy.example.invalid/v1"); err != nil || len(agent.asked) != 1 || agent.asked[0] != [2]string{"gpt-5.6", "acme-http"} {
		t.Fatalf("responses predicate asked %v %v", agent.asked, err)
	}
	if _, err := FallbackAPIModeResolved(ctx, &fakeFallbackModeAgent{}, "custom", "m", "http://[::1"); err == nil {
		t.Fatal("a base url urlparse rejects must raise")
	}
	if got, err := FallbackAPIModeResolved(auxMainTestHome(t, ""), &fakeFallbackModeAgent{}, "nous", "anthropic/claude-opus-4.6", ""); err != nil || got != "chat_completions" {
		t.Fatalf("default nous wire got %q %v", got, err)
	}
}

func TestFallbackAPIModeHintPrefersExplicitThenDeclaredThenAnthropicSignal(t *testing.T) {
	ctx := auxMainTestHome(t, "providers:\n  relay:\n    base_url: https://relay.example.com/v1\n    api_mode: codex_responses\n  plain:\n    base_url: https://plain.example.com/v1\n")
	anthropicBase := "https://relay.example.com/anthropic"
	plainBase := "https://plain.example.com/v1"
	cases := []struct {
		name     string
		fb       map[string]any
		provider string
		hint     *string
		explicit bool
		want     string
	}{
		{"explicit api_mode wins even as chat", map[string]any{"api_mode": "chat_completions"}, "anthropic", &anthropicBase, true, "chat_completions"},
		{"transport spelling", map[string]any{"transport": "responses"}, "custom", nil, true, "codex_responses"},
		{"invalid explicit falls through", map[string]any{"api_mode": "bogus"}, "custom", &plainBase, false, "chat_completions"},
		{"declared provider wire", map[string]any{}, "relay", &plainBase, true, "codex_responses"},
		{"provider without a declared wire", map[string]any{}, "plain", &plainBase, false, "chat_completions"},
		{"anthropic provider without a base url", map[string]any{}, "anthropic", nil, false, "anthropic_messages"},
		{"anthropic wire base url", map[string]any{}, "custom", &anthropicBase, false, "anthropic_messages"},
		{"moa skips the declared lookup", map[string]any{}, "moa", nil, false, "chat_completions"},
	}
	for _, c := range cases {
		explicit, mode, err := FallbackAPIModeHint(ctx, c.fb, c.provider, c.hint)
		if err != nil || explicit != c.explicit || mode != c.want {
			t.Fatalf("%s: got %v %q %v", c.name, explicit, mode, err)
		}
	}
}
