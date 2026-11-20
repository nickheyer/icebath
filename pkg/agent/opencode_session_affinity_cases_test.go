package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func opencodeAffinityAgent(t *testing.T, providerName, model, baseURL, apiMode string) *AIAgent {
	t.Helper()
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: baseURL, Model: model, Provider: providerName, SessionID: "sess-affinity-1"})
	if apiMode != "" {
		a.apiMode = apiMode
		a.anthropicBaseURL = baseURL
	}
	return a
}

func TestMainTurnSendsStableSessionHeaderOnEveryTransport(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": "hi"}}
	rows := []struct{ provider, model, baseURL, apiMode, wantMode string }{
		{"opencode-go", "glm-5", "https://opencode.ai/zen/go/v1", "", "chat_completions"},
		{"opencode-go", "gpt-5.6-luna", "https://opencode.ai/zen/go/v1", "", "codex_responses"},
		{"opencode-go", "minimax-m2.7", "https://opencode.ai/zen/go/v1", "anthropic_messages", "anthropic_messages"},
		{"custom", "glm-5", "https://opencode.ai/zen/go/v1", "", "chat_completions"},
	}
	for _, row := range rows {
		a := opencodeAffinityAgent(t, row.provider, row.model, row.baseURL, row.apiMode)
		if a.apiMode != row.wantMode {
			t.Fatalf("%s %s resolved api mode %q want %q", row.provider, row.model, a.apiMode, row.wantMode)
		}
		first := mainTurnHeaders(t, a, msgs)[provider.OpencodeSessionHeader]
		second := mainTurnHeaders(t, a, msgs)[provider.OpencodeSessionHeader]
		if first != "sess-affinity-1" || second != "sess-affinity-1" {
			t.Fatalf("%s %s via %s sent %v then %v", row.provider, row.model, a.apiMode, first, second)
		}
	}
	other := opencodeAffinityAgent(t, "openrouter", "anthropic/claude-sonnet-4.6", "https://openrouter.ai/api/v1", "")
	if headers := mainTurnHeaders(t, other, msgs); headers[provider.OpencodeSessionHeader] != nil {
		t.Fatalf("openrouter route carried %v", headers)
	}
}
