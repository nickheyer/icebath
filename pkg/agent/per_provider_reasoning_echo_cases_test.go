package agent

import (
	"strings"
	"testing"
)

// Minimal agent of the custom gateway route with the given echo opt in
func echoRouteAgent(t *testing.T, flag bool) *AIAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	base := "https://gw.example.com/v1"
	return &AIAgent{provider: "custom", model: "my-model", baseURL: base, baseURLLower: strings.ToLower(base), reasoningEchoFlag: flag}
}

func TestStrictFallbackStripsDespitePrimaryOptIn(t *testing.T) {
	a := echoRouteAgent(t, true)
	a.provider, a.model = "mistral", "mistral-large"
	a.baseURL, a.baseURLLower = "https://api.mistral.ai/v1", "https://api.mistral.ai/v1"
	a.reasoningRouteCache = nil
	a.reasoningEchoFlag = false
	if a.needsThinkingReasoningPad() {
		t.Fatal("a strict fallback must not pad")
	}
	apiMsgs := []map[string]any{
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": "hi", "reasoning_content": "thoughts"},
		{"role": "assistant", "content": "hi2", "reasoning_content": " "},
		{"role": "user", "content": "bye"},
	}
	if changed := a.reapplyReasoningEchoForProvider(apiMsgs); changed != 2 {
		t.Fatalf("changed %d", changed)
	}
	for _, m := range apiMsgs {
		if _, has := m["reasoning_content"]; m["role"] == "assistant" && has {
			t.Fatalf("reasoning_content survived on %v", m)
		}
	}
}

func TestFallbackOptInPreservesReasoning(t *testing.T) {
	a := echoRouteAgent(t, false)
	a.provider, a.model = "custom", "t9s/kimi-k3"
	a.reasoningRouteCache = nil
	a.reasoningEchoFlag = true
	if !a.needsThinkingReasoningPad() {
		t.Fatal("an opted in fallback must pad")
	}
	apiMsgs := []map[string]any{
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": "hi"},
		{"role": "user", "content": "bye"},
	}
	if changed := a.reapplyReasoningEchoForProvider(apiMsgs); changed != 1 {
		t.Fatalf("changed %d", changed)
	}
	if apiMsgs[1]["reasoning_content"] != " " {
		t.Fatalf("pad %q", apiMsgs[1]["reasoning_content"])
	}
}
