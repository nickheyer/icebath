package slack

import (
	"context"
	"testing"
)

// Adapter with the agent sessions API pinned on for the case
func agentSessionsAdapter(t *testing.T) (*SlackAdapter, *fakeSlackAPI) {
	prev := agentSessionsSupported
	agentSessionsSupported = true
	t.Cleanup(func() { agentSessionsSupported = prev })
	a, f, _ := newHarnessAdapter(t, nil)
	return a, f
}

// Single call of the method whose arguments must equal want
func assertOneCall(t *testing.T, f *fakeSlackAPI, method string, want map[string]any) {
	t.Helper()
	calls := f.callsTo(method)
	if len(calls) != 1 {
		t.Fatalf("%s calls %v", method, calls)
	}
	for k, v := range want {
		if calls[0].Args[k] != v {
			t.Fatalf("%s args %v want %v", method, calls[0].Args, want)
		}
	}
	if len(calls[0].Args) != len(want) {
		t.Fatalf("%s args %v want %v", method, calls[0].Args, want)
	}
}

func TestAgentSessionsApiRoutingTypingUsesAgentSessionsWhenSupported(t *testing.T) {
	a, f := agentSessionsAdapter(t)
	if err := a.SendTyping(context.Background(), "C123", map[string]any{"thread_id": "parent_ts"}); err != nil {
		t.Fatal(err)
	}
	assertOneCall(t, f, "agents.sessions.setStatus", map[string]any{"channel_id": "C123", "thread_ts": "parent_ts", "status": "is thinking..."})
	if n := len(f.callsTo("assistant.threads.setStatus")); n != 0 {
		t.Fatalf("legacy status calls %d", n)
	}
}

func TestAgentSessionsApiRoutingStopTypingClearsViaAgentSessions(t *testing.T) {
	a, f := agentSessionsAdapter(t)
	ctx := context.Background()
	if err := a.SendTyping(ctx, "C123", map[string]any{"thread_id": "parent_ts"}); err != nil {
		t.Fatal(err)
	}
	before := len(f.callsTo("agents.sessions.setStatus"))
	if err := a.StopTyping(ctx, "C123", map[string]any{"thread_id": "parent_ts"}); err != nil {
		t.Fatal(err)
	}
	calls := f.callsTo("agents.sessions.setStatus")[before:]
	if len(calls) != 1 || calls[0].Args["channel_id"] != "C123" || calls[0].Args["thread_ts"] != "parent_ts" || calls[0].Args["status"] != "" || len(calls[0].Args) != 3 {
		t.Fatalf("clear calls %v", calls)
	}
	if n := len(f.callsTo("assistant.threads.setStatus")); n != 0 {
		t.Fatalf("legacy status calls %d", n)
	}
}

func TestAgentSessionsApiRoutingThreadTitleUsesAgentsSessionsRename(t *testing.T) {
	a, f := agentSessionsAdapter(t)
	a.Config.Extra["assistant_thread_titles"] = true
	a.setAssistantThreadTitle(context.Background(), "D123", "171234.0001", "Summarize the incident", "")
	assertOneCall(t, f, "agents.sessions.rename", map[string]any{"channel_id": "D123", "thread_ts": "171234.0001", "title": "Summarize the incident"})
	if n := len(f.callsTo("assistant.threads.setTitle")); n != 0 {
		t.Fatalf("legacy title calls %d", n)
	}
}
