package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	sessionHeaderBase = "http://localhost:4000/v1"
	sessionHeaderName = "x-litellm-session-id"
)

var sessionHeaderMsgs = []map[string]any{{"role": "user", "content": "hello"}}

func sessionHeaderConfig(withHeader bool) string {
	cfg := "custom_providers:\n  - name: litellm-lan\n    provider_key: litellm-lan\n    base_url: " + sessionHeaderBase + "\n"
	if withHeader {
		cfg += "    session_affinity_header: " + sessionHeaderName + "\n"
	}
	return cfg
}

func sessionHeaderAgent(t *testing.T, withHeader bool, sessionID, apiMode string) *AIAgent {
	t.Helper()
	a := newFixtureAgentWithConfig(t, sessionHeaderConfig(withHeader), AIAgentOptions{BaseURL: sessionHeaderBase, Model: "claude-3-5-sonnet",
		Provider: "litellm-lan", APIMode: apiMode, SessionID: sessionID})
	a.anthropicBaseURL = sessionHeaderBase
	return a
}

func extraHeaderMap(raw any) map[string]any {
	out := map[string]any{}
	switch h := raw.(type) {
	case map[string]any:
		for k, v := range h {
			out[k] = v
		}
	case map[string]string:
		for k, v := range h {
			out[k] = v
		}
	}
	return out
}

func mainTurnHeaders(t *testing.T, a *AIAgent, msgs []map[string]any) map[string]any {
	t.Helper()
	kwargs, err := a.BuildAPIKwargs(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return extraHeaderMap(kwargs["extra_headers"])
}

func auxTurnHeaders(t *testing.T, sessionID string) map[string]any {
	t.Helper()
	ctx := provider.SetRuntimeMain(t.Context(), "litellm-lan", "claude-3-5-sonnet", provider.RuntimeMainOptions{BaseURL: sessionHeaderBase, SessionID: sessionID})
	kwargs, err := provider.AuxCallKwargs(ctx, "litellm-lan", "claude-3-5-sonnet", []any{map[string]any{"role": "user", "content": "hello"}}, sessionHeaderBase)
	if err != nil {
		t.Fatal(err)
	}
	return extraHeaderMap(kwargs["extra_headers"])
}

func TestConfiguredHeaderCarriesOneValuePerConversationOnEveryPath(t *testing.T) {
	chat := mainTurnHeaders(t, sessionHeaderAgent(t, true, "sess-A", "chat_completions"), sessionHeaderMsgs)[sessionHeaderName]
	anthropic := mainTurnHeaders(t, sessionHeaderAgent(t, true, "sess-A", "anthropic_messages"), sessionHeaderMsgs)[sessionHeaderName]
	aux := auxTurnHeaders(t, "sess-A")[sessionHeaderName]
	if chat != "sess-A" || anthropic != "sess-A" || aux != "sess-A" {
		t.Fatalf("chat %v anthropic %v aux %v want sess-A on every path", chat, anthropic, aux)
	}
	if got := mainTurnHeaders(t, sessionHeaderAgent(t, true, "sess-B", "chat_completions"), sessionHeaderMsgs)[sessionHeaderName]; got != "sess-B" {
		t.Fatalf("second conversation header %v", got)
	}
	pinned := sessionHeaderAgent(t, true, "sess-A", "chat_completions")
	pinned.requestOverrides = map[string]any{"extra_headers": map[string]any{sessionHeaderName: "pinned-by-caller"}}
	if got := mainTurnHeaders(t, pinned, sessionHeaderMsgs)[sessionHeaderName]; got != "pinned-by-caller" {
		t.Fatalf("caller pinned header %v", got)
	}
}

func TestUnconfiguredProviderSendsNoSessionHeader(t *testing.T) {
	for _, mode := range []string{"chat_completions", "anthropic_messages"} {
		headers := mainTurnHeaders(t, sessionHeaderAgent(t, false, "sess-A", mode), sessionHeaderMsgs)
		if _, ok := headers[sessionHeaderName]; ok {
			t.Fatalf("%s sent %s without configuration: %v", mode, sessionHeaderName, headers)
		}
		if _, ok := headers[provider.OpencodeSessionHeader]; ok {
			t.Fatalf("%s sent %s for a non OpenCode route: %v", mode, provider.OpencodeSessionHeader, headers)
		}
	}
	if headers := auxTurnHeaders(t, "sess-A"); headers[sessionHeaderName] != nil {
		t.Fatalf("aux path sent %s without configuration: %v", sessionHeaderName, headers)
	}
}
