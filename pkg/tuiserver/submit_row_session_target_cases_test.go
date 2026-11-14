package tuiserver

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Production rotation onto a continuation, the parent reopened the way a resume of the old id does
func submitRowRotateToChild(t *testing.T, db *state.SessionDB, parent string, a *agent.AIAgent, reopenParent bool) string {
	t.Helper()
	ctx := context.Background()
	child, model := state.NewSessionID(), "test-model"
	if err := db.PublishCompressionChild(ctx, state.CompressionChildPublish{ParentSessionID: parent, ChildSessionID: child, Source: "desktop", Model: &model,
		Messages: []map[string]any{{"role": "user", "content": "earlier turn"}, {"role": "assistant", "content": "earlier reply"}}}); err != nil {
		t.Fatal(err)
	}
	a.SetSessionID(child)
	if reopenParent {
		if err := db.ReopenSession(ctx, parent); err != nil {
			t.Fatal(err)
		}
	}
	return child
}

// Role and the first 48 characters of every row of the session, inactive rows included
func submitRowRows(t *testing.T, db *state.SessionDB, key string) [][2]string {
	t.Helper()
	rows, err := db.GetMessagesAsConversation(context.Background(), key, state.ConversationOptions{IncludeInactive: true})
	if err != nil {
		t.Fatal(err)
	}
	out := make([][2]string, 0, len(rows))
	for _, r := range rows {
		content := []rune(strOr(r["content"]))
		if len(content) > 48 {
			content = content[:48]
		}
		out = append(out, [2]string{strOr(r["role"]), string(content)})
	}
	return out
}

func TestSubmitRowSessionTarget(t *testing.T) {
	t.Run("test_submit_user_row_lands_where_the_turns_tool_rows_land", func(t *testing.T) {
		sessionCaseScope(t)
		ctx := context.Background()
		db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		sid, key := submitRowDesktopSession(t, db)
		session := tuigateway.SessionRecord(sid)
		if session == nil {
			t.Fatalf("no live session %s", sid)
		}
		session.Set("running", true)
		startInflightTurn(session, "earlier turn", "", nil)
		if !ensureSessionDBRow(ctx, session) {
			t.Fatal("parent row not ensured")
		}
		a := submitRowFlushAgent(t, db, key)
		session.Set("agent", a)
		child := submitRowRotateToChild(t, db, key, a, true)
		if session.Get("session_key") != key || a.SessionID() != child {
			t.Fatalf("session key %v agent session %s", session.Get("session_key"), a.SessionID())
		}
		session.Set("running", true)
		startInflightTurn(session, "Lets make things right", "", nil)
		if errResp := persistSessionRowForSubmit(ctx, "rid", session, "Lets make things right", ""); errResp != nil {
			t.Fatalf("submit persist %v", errResp)
		}
		adoptSubmitUserRow(ctx, session, a, "Lets make things right", "Lets make things right")
		userMsg, _ := a.StageTurnUserMessage("Lets make things right", "Lets make things right")
		messages := []map[string]any{userMsg}
		a.SetPersistUserMessageIdx(0)
		if outcome := a.FlushMessagesToSessionDB(messages); outcome == state.FlushFailed {
			t.Fatalf("flush outcome %v", outcome)
		}
		turnEnd := append(messages,
			map[string]any{"role": "assistant", "content": "calling a tool", "tool_calls": []any{
				map[string]any{"id": "t1", "type": "function", "function": map[string]any{"name": "memory", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "t1", "name": "memory", "content": "failed write"},
			map[string]any{"role": "assistant", "content": "done"})
		if outcome := a.FlushMessagesToSessionDB(turnEnd); outcome == state.FlushFailed {
			t.Fatalf("flush outcome %v", outcome)
		}
		for _, r := range submitRowRows(t, db, key) {
			if r == [2]string{"user", "Lets make things right"} {
				t.Fatalf("user row written to the rotated-away parent %s", key)
			}
		}
		want := [][2]string{{"user", "earlier turn"}, {"assistant", "earlier reply"}, {"user", "Lets make things right"},
			{"assistant", "calling a tool"}, {"tool", "failed write"}, {"assistant", "done"}}
		if got := submitRowRows(t, db, child); !reflect.DeepEqual(got, want) {
			t.Fatalf("the turn's rows must follow the agent session into the continuation %v", got)
		}
	})
}
