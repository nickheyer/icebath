package tuiserver

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

const closingSID = "closing-sid"

// Session record of the closing cases with the overrides
func closingSession(extra map[string]any) *tuihost.Session {
	fields := map[string]any{"agent": nil, "session_key": "closing-session-key", "history": []any{}, "history_lock": &sync.Mutex{},
		"history_version": int64(0), "running": false, "attached_images": []any{}, "image_counter": 0, "cols": 80,
		"show_reasoning": false, "tool_progress_mode": "all"}
	for key, value := range extra {
		fields[key] = value
	}
	return tuihost.NewSession(fields)
}

// Real agent of the closing key on a scripted provider whose requests count the turns run
func closingBuiltAgent(t *testing.T) (*agent.AIAgent, *fakellm.Server) {
	t.Helper()
	srv, err := fakellm.New([]any{fakellm.Text{Text: "done"}}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	return testAgent(t, agent.AIAgentOptions{Model: fakellm.ModelID, Provider: "custom", BaseURL: srv.BaseURL(), APIMode: "chat_completions",
		SessionID: "closing-session-key", Platform: "tui"}), srv
}

func closingRPC(method string, params map[string]any) map[string]any {
	merged := map[string]any{"session_id": closingSID}
	for key, value := range params {
		merged[key] = value
	}
	return handleRequest(context.Background(), map[string]any{"id": method, "method": method, "params": merged})
}

// Session ids holding a registry lease of the home
func leaseIDs(t *testing.T, h string) []any {
	t.Helper()
	var ids []any
	for _, entry := range registryEntries(t, h) {
		ids = append(ids, entry["session_id"])
	}
	return ids
}

// Refusal a new runtime of the same chat gets, a successful claim released again
func reopenRefusal(ctx context.Context) *activesessions.Refusal {
	reopened := closingSession(nil)
	refusal := ensureActiveSessionSlot(ctx, "reopened-sid", reopened)
	releaseActiveSessionSlot(reopened)
	return refusal
}

// Waits until the turn thread of the session ended and the session stopped running
func joinTurn(t *testing.T, session *tuihost.Session) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		thread, _ := session.Get("_run_thread").(*memory.ContextThread)
		if (thread == nil || !thread.Alive()) && !truthy(session.Get("running")) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("turn thread still alive")
		}
	}
}
