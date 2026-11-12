package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Real agent bound to a real session store with the early flush bookkeeping of a fresh session
func earlyFlushAgent(t *testing.T, sid string) (*AIAgent, *state.SessionDB) {
	t.Helper()
	db := restoreRealDB(t, sid, "cli")
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", Platform: "cli", SessionDB: db, SessionID: sid, EnabledToolsets: []string{}})
	t.Cleanup(func() { a.Close() })
	a.sessionDBCreated, a.cachedSystemPrompt, a.skipMCPRefresh, a.compressionEnabled = true, "SYSTEM", true, false
	a.memoryManager, a.memoryStore, a.memoryNudgeInterval = nil, nil, 0
	return a, db
}

func storedRows(t *testing.T, db *state.SessionDB, sid string) []*pyjson.Object {
	t.Helper()
	rows, err := db.GetMessages(t.Context(), sid, state.GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Reads an integer of any Go width, false for every other type the way isinstance int checks
func intLike(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

func rowField(row *pyjson.Object, key string) any {
	v, _ := row.Get(key)
	return v
}

func TestRealEarlyFlushAndOverrideLifecycle(t *testing.T) {
	caseHome(t)
	t.Run("test_real_close_flush_syncs_row_id_and_prologue_backfills", func(t *testing.T) {
		sid := "sess-real-flush"
		a, db := earlyFlushAgent(t, sid)
		staged := map[string]any{"role": "user", "content": "hello"}
		a.pendingCLIUserMessage = staged
		flushed, err := a.flushMessagesToSessionDB([]map[string]any{staged})
		if err != nil || !flushed {
			t.Fatalf("flush %v %v", flushed, err)
		}
		rows := storedRows(t, db, sid)
		last := rows[len(rows)-1]
		if staged["_db_persisted"] != true {
			t.Fatalf("staged %v", staged)
		}
		rowID, isInt := intLike(staged["_row_id"])
		if storedID, _ := intLike(rowField(last, "id")); !isInt || rowID != storedID {
			t.Fatalf("row id %#v stored %#v", staged["_row_id"], rowField(last, "id"))
		}
		if rowField(last, "api_content") != nil {
			t.Fatalf("api_content before the prologue %#v", rowField(last, "api_content"))
		}
		patchPluginHooks(t, func(string, map[string]any) []any {
			return []any{map[string]any{"context": "PLUGIN-CTX"}}
		})
		tc := prologueBuild(t, a, nil)
		expected, _ := composeUserAPIContent("hello", "", "PLUGIN-CTX")
		if got := tc.Messages[tc.CurrentTurnUserIdx].APIContent; got != expected {
			t.Fatalf("live sidecar %q want %q", got, expected)
		}
		rows = storedRows(t, db, sid)
		if got := rowField(rows[len(rows)-1], "api_content"); got != expected {
			t.Fatalf("stored sidecar %#v want %q", got, expected)
		}
	})
	t.Run("test_pre_flushed_api_only_turn_without_injections_preserves_sidecar", func(t *testing.T) {
		sid := "sess-api-only-no-inj"
		a, db := earlyFlushAgent(t, sid)
		cleanText, apiText := "hello", "[voice] hello"
		staged := map[string]any{"role": "user", "content": cleanText}
		a.pendingCLIUserMessage = staged
		if _, err := a.flushMessagesToSessionDB([]map[string]any{staged}); err != nil {
			t.Fatal(err)
		}
		if staged["_row_id"] == nil {
			t.Fatalf("staged %v", staged)
		}
		patchPluginHooks(t, func(string, map[string]any) []any { return nil })
		tc := prologueBuild(t, a, func(r *TurnContextRequest) { r.UserMessage, r.PersistUserMessage = apiText, cleanText })
		turn := tc.Messages[tc.CurrentTurnUserIdx]
		if turn.Content != apiText || turn.APIContent != apiText {
			t.Fatalf("live content %#v sidecar %q", turn.Content, turn.APIContent)
		}
		rows := storedRows(t, db, sid)
		if len(rows) != 1 || rowField(rows[0], "content") != cleanText || rowField(rows[0], "api_content") != apiText {
			t.Fatalf("stored rows %v", rows)
		}
		conv := persistConversation(t, db, sid, false)
		if conv[0]["content"] != cleanText || conv[0]["api_content"] != apiText {
			t.Fatalf("replayed %v", conv[0])
		}
		substituteAPIContent(conv[0])
		if conv[0]["content"] != apiText {
			t.Fatalf("substituted %v", conv[0])
		}
	})
}
