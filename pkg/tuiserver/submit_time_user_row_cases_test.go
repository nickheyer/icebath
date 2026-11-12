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

// Desktop session created through the RPC over the shared store, its live id and stored key
func submitRowDesktopSession(t *testing.T, db *state.SessionDB) (string, string) {
	t.Helper()
	useSharedDB(t, db)
	resp := lookupMethod("session.create")(context.Background(), "c", map[string]any{"cols": 96, "source": "desktop"})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("create %v", resp)
	}
	return strOr(result["session_id"]), strOr(result["stored_session_id"])
}

// Agent owning the real flush on the stored session key, the crash persist at turn start runs the same code
func submitRowFlushAgent(t *testing.T, db *state.SessionDB, key string) *agent.AIAgent {
	t.Helper()
	h := t.TempDir()
	a, err := agent.NewAIAgent(context.Background(), agent.AIAgentOptions{APIKey: "test-key-1234567890", BaseURL: "https://openrouter.ai/api/v1",
		Model: "test/model", SessionDB: db, SessionID: key, QuietMode: true, SkipContextFiles: true, SkipMemory: true,
		EnabledToolsets: []string{}, Cwd: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestSubmitTimeUserRow(t *testing.T) {
	t.Run("test_turn_adopts_the_submit_row_and_writes_no_duplicate", func(t *testing.T) {
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
		startInflightTurn(session, "look at @notes.md", "", nil)
		if errResp := persistSessionRowForSubmit(ctx, "rid", session, "look at @notes.md", ""); errResp != nil {
			t.Fatalf("submit persist %v", errResp)
		}
		a := submitRowFlushAgent(t, db, key)
		expanded := "look at @notes.md\n\n<file notes.md>todo</file>"
		adoptSubmitUserRow(ctx, session, a, expanded, "look at @notes.md")
		if _, held := session.Lookup("_submit_user_row"); held {
			t.Fatal("the submit row stayed on the session")
		}
		userMsg, _ := a.StageTurnUserMessage(expanded, expanded)
		if pending := a.PendingCLIUserMessage(); pending == nil || reflect.ValueOf(userMsg).Pointer() != reflect.ValueOf(pending).Pointer() {
			t.Fatalf("the turn rebuilt the row instead of adopting it %v %v", userMsg, pending)
		}
		messages := []map[string]any{userMsg}
		a.SetPersistUserMessageIdx(0)
		if _, err := a.FlushMessagesToSessionDB(messages); err != nil {
			t.Fatal(err)
		}
		if _, err := a.FlushMessagesToSessionDB(append(messages, map[string]any{"role": "assistant", "content": "done"})); err != nil {
			t.Fatal(err)
		}
		rows, err := db.GetMessagesAsConversation(ctx, key, state.ConversationOptions{IncludeInactive: true})
		if err != nil {
			t.Fatal(err)
		}
		var got [][2]any
		for _, r := range rows {
			got = append(got, [2]any{r["role"], r["content"]})
		}
		if want := [][2]any{{"user", expanded}, {"assistant", "done"}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("rows %v want %v", got, want)
		}
	})
}
