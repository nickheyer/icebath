package tuiserver

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Memory provider counting the session end commits it receives
type sessionEndRecorder struct {
	memory.BaseProvider
	ends atomic.Int32
}

func (p *sessionEndRecorder) Name() string                   { return "session-end-recorder" }
func (p *sessionEndRecorder) Available(context.Context) bool { return true }
func (p *sessionEndRecorder) Initialize(context.Context, string, memory.InitOptions) error {
	return nil
}
func (p *sessionEndRecorder) ToolSchemas(context.Context) ([]map[string]any, error) { return nil, nil }
func (p *sessionEndRecorder) OnSessionEnd(context.Context, []map[string]any) error {
	p.ends.Add(1)
	return nil
}

// Real agent on a store with the recording memory provider, the surface the finalize cases drive
func persistAgent(t *testing.T, db *state.DB, sessionID, model string) (*agent.AIAgent, *sessionEndRecorder) {
	t.Helper()
	recorder := &sessionEndRecorder{}
	mm := memory.NewMemoryManager(context.Background(), nil, nil)
	if err := mm.AddProvider(context.Background(), recorder); err != nil {
		t.Fatal(err)
	}
	a, err := agent.NewAIAgent(context.Background(), agent.AIAgentOptions{Model: model, Provider: "custom", BaseURL: "http://127.0.0.1:9/v1",
		APIKey: "k", APIMode: "chat_completions", SessionID: sessionID, Platform: "tui", QuietMode: true, SkipContextFiles: true,
		SessionDB: db, MemoryManager: mm})
	if err != nil {
		t.Fatal(err)
	}
	return a, recorder
}

// Store under a fresh home that the server shares for the case
func persistDB(t *testing.T) *state.DB {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(h, ".icebath"))
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	useSharedDB(t, db)
	return db
}

// Live session record of the finalize cases
func persistSession(a *agent.AIAgent, history []any, sessionKey string) *tuigateway.Session {
	return tuigateway.NewSession(map[string]any{"agent": a, "history": history, "session_key": sessionKey, "_finalized": false})
}

func TestFinalizeSessionPersistCommitMemoryStillCalled(t *testing.T) {
	db := persistDB(t)
	a, recorder := persistAgent(t, db, "test_session_001", "test-model")
	finalizeSession(context.Background(), persistSession(a, []any{map[string]any{"role": "user", "content": "x"}}, "test_key_001"), "tui_close")
	if n := recorder.ends.Load(); n != 1 {
		t.Fatalf("memory commits %d", n)
	}
}

func TestFinalizeSessionPersistAlreadyFinalizedSkips(t *testing.T) {
	db := persistDB(t)
	if err := db.CreateSession(state.NewSession{ID: "test_session_001", Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	a, recorder := persistAgent(t, db, "test_session_001", "test-model")
	a.SetUnflushedSessionMessagesForTests([]map[string]any{{"role": "user", "content": "x"}})
	session := persistSession(a, []any{map[string]any{"role": "user", "content": "x"}}, "test_key_001")
	session.Set("_finalized", true)
	finalizeSession(context.Background(), session, "tui_close")
	after, err := db.GetMessagesAsConversation(context.Background(), "test_session_001", state.ConversationOptions{})
	if err != nil || len(after) != 0 || recorder.ends.Load() != 0 {
		t.Fatalf("persisted %v %v commits %d", after, err, recorder.ends.Load())
	}
}

func TestUnflushedTurnSurvivesDisconnect(t *testing.T) {
	db := persistDB(t)
	sessionID := "sess-unflushed"
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	turn := []map[string]any{{"role": "user", "content": "scan the repo and summarise"}, {"role": "assistant", "content": "Here is the summary…"},
		{"role": "user", "content": "now open a PR"}, {"role": "assistant", "content": "PR opened."}}
	a, _ := persistAgent(t, db, sessionID, "test-model")
	a.SetUnflushedSessionMessagesForTests(turn)
	history := make([]any, len(turn))
	for i, m := range turn {
		history[i] = m
	}
	if before, _ := db.GetMessagesAsConversation(context.Background(), sessionID, state.ConversationOptions{}); len(before) != 0 {
		t.Fatalf("before %v", before)
	}
	finalizeSession(context.Background(), persistSession(a, history, sessionID), "ws_disconnect")
	after, err := db.GetMessagesAsConversation(context.Background(), sessionID, state.ConversationOptions{})
	if err != nil || len(after) != 4 {
		t.Fatalf("after %v %v", after, err)
	}
	var joined []string
	for _, m := range after {
		joined = append(joined, strOr(m["content"]))
	}
	text := strings.Join(joined, "\n")
	if !strings.Contains(text, "scan the repo") || !strings.Contains(text, "PR opened") {
		t.Fatalf("contents %v", joined)
	}
}

func TestResumedSessionNotReflushedAsDuplicates(t *testing.T) {
	db := persistDB(t)
	sessionID := "sess-resumed"
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	loaded := []any{map[string]any{"role": "user", "content": "hello"}, map[string]any{"role": "assistant", "content": "hi there"}}
	for _, m := range loaded {
		if _, err := db.AppendMessage(context.Background(), sessionID, m.(map[string]any), state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := persistAgent(t, db, sessionID, "test-model")
	finalizeSession(context.Background(), persistSession(a, loaded, sessionID), "ws_disconnect")
	after, err := db.GetMessagesAsConversation(context.Background(), sessionID, state.ConversationOptions{})
	if err != nil || len(after) != 2 {
		t.Fatalf("after %v %v", after, err)
	}
}

func TestOnSessionEndHookFiredWithInterruptedTrue(t *testing.T) {
	db := persistDB(t)
	var calls []map[string]any
	recordHook(context.Background(), "on_session_end", func(_ context.Context, kwargs map[string]any) error {
		calls = append(calls, kwargs)
		return nil
	})
	a, _ := persistAgent(t, db, "hook_test_001", "claude-sonnet-4")
	finalizeSession(context.Background(), persistSession(a, []any{map[string]any{"role": "user", "content": "test"}}, "test_key_001"), "tui_close")
	want := map[string]any{"session_id": "hook_test_001", "completed": false, "interrupted": true, "model": "claude-sonnet-4", "platform": "tui"}
	for _, call := range calls {
		if reflect.DeepEqual(call, want) {
			return
		}
	}
	t.Fatalf("on_session_end calls %v", calls)
}
