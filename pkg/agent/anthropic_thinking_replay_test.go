package agent

import "testing"

func TestRejectedThinkingTrackerBindsRouteAndCompressor(t *testing.T) {
	c := &ContextCompressor{}
	a := &AIAgent{apiMode: "anthropic_messages", baseURL: "https://api.anthropic.com", model: "claude-opus-4-6",
		sessionID: "s1", contextCompressor: c}
	tr := a.rejectedThinkingTracker()
	if c.rejectedThinking != tr || tr.SessionID != "s1" || !tr.TracksRejectedThinking() || tr.Store != nil {
		t.Fatalf("tracker %+v", tr)
	}
	a.sessionID = "s2"
	if a.rejectedThinkingTracker() != tr || tr.SessionID != "s2" {
		t.Fatal("tracker not rebound to the rotated session")
	}
	msgs := []map[string]any{{"role": "assistant", "content": "a", "reasoning_details": []any{
		map[string]any{"type": "thinking", "thinking": "t", "signature": "sig"}}}}
	if n := applyRejectedThinkingSuppression(a, msgs); n != 0 || len(msgs[0]["reasoning_details"].([]any)) != 1 {
		t.Fatalf("suppressed %d without rejections", n)
	}
	a.apiMode = "chat_completions"
	if a.rejectedThinkingTracker().TracksRejectedThinking() {
		t.Fatal("chat completions route tracks signed replay")
	}
}
