package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Rotation agent of the hermes builder on a real store, its engine folding into the given rows
func rotationDedupAgent(t *testing.T, db *state.SessionDB, sessionID string, fold []map[string]any) *AIAgent {
	t.Helper()
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", Platform: "telegram", SessionDB: db, SessionID: sessionID})
	t.Cleanup(func() { a.Close() })
	engine := &scriptedEngine{stubEngine: *newStubEngine(200000, 0.5), compress: func([]map[string]any) []map[string]any { return fold }}
	engine.CompressionCount = 1
	a.contextCompressor = engine
	a.compressionInPlace = false
	return a
}

// Makes every message write of one session raise the way the patched parent flush does, until the returned undo runs
func failSessionWrites(t *testing.T, db *state.SessionDB, sessionID string) func() {
	t.Helper()
	exec := func(query string) {
		if err := db.ExecuteWrite(t.Context(), func(conn state.SQLiteConn) error {
			_, err := conn.ExecContext(t.Context(), query)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE TRIGGER fail_parent_flush BEFORE INSERT ON messages WHEN NEW.session_id = '" + sessionID + "' BEGIN SELECT RAISE(ABORT, 'simulated parent flush failure'); END")
	return func() { exec("DROP TRIGGER fail_parent_flush") }
}

// Counts rows matching the content and role when given, the hermes _count_rows helper
func countRows(rows []map[string]any, content any, role string) int {
	n := 0
	for _, r := range rows {
		if content != nil && !reflect.DeepEqual(r["content"], content) {
			continue
		}
		if role != "" && r["role"] != role {
			continue
		}
		n++
	}
	return n
}

func TestRotationChildFlushDedup(t *testing.T) {
	caseHome(t)
	t.Run("test_rotation_stamps_anchor_source_when_reanchor_fallback_rewrote_turn", func(t *testing.T) {
		ctx := context.Background()
		parent := "PARENT_ROT_REANCHOR_DRIFT"
		db := restoreRealDB(t, parent, "cli")
		for _, r := range [][2]string{{"user", "old durable"}, {"assistant", "persisted answer"}} {
			if _, err := db.AppendMessage(ctx, parent, map[string]any{"role": r[0], "content": r[1]}, state.AppendGuards{}); err != nil {
				t.Fatal(err)
			}
		}
		loaded, err := db.GetMessagesAsConversation(ctx, parent, state.ConversationOptions{})
		if err != nil {
			t.Fatal(err)
		}
		messages := []map[string]any{
			{"role": "user", "content": "old durable"},
			{"role": "user", "content": "current ask\n\n[merged summary]"},
			{"role": "user", "content": "Current todos:\n- [ ] leftover", "_todo_snapshot_synthetic": true},
		}
		live := make([]*state.Message, len(messages))
		for i, m := range messages {
			live[i] = state.MessageFromDict(m)
		}
		drifted := reanchorCurrentTurnUserIdx(live, "current ask")
		anchorSource := -1
		for i, m := range messages {
			if isRealUserMessage(m) {
				anchorSource = i
			}
		}
		if drifted != 2 || anchorSource != 1 {
			t.Fatalf("drifted %d anchor source %d", drifted, anchorSource)
		}
		a := rotationDedupAgent(t, db, parent, []map[string]any{{"role": "user", "content": "handoff scaffolding", "_todo_snapshot_synthetic": true}})
		a.persistUserMessageIdx = &drifted
		undo := failSessionWrites(t, db, parent)
		approx, system := 120000, "sys"
		if _, _, err := a.compressContext(ctx, messages, &system, compressContextOptions{ApproxTokens: &approx, CommitFence: NewCompressionCommitFence()}); err != nil {
			t.Fatal(err)
		}
		undo()
		if _, ok := messages[1][dbPersistedMarker]; !ok {
			t.Fatalf("carrier unstamped %v", messages[1])
		}
		if _, ok := messages[2][dbPersistedMarker]; ok {
			t.Fatalf("synthetic snapshot row stamped %v", messages[2])
		}
		if outcome := a.flushMessagesToSessionDBWithHistory(messages, loaded); outcome == state.FlushFailed {
			t.Fatalf("flush outcome %v", outcome)
		}
		childRows, err := db.GetMessagesAsConversation(ctx, a.sessionID, state.ConversationOptions{IncludeInactive: true})
		if err != nil {
			t.Fatal(err)
		}
		if n := countRows(childRows, "current ask\n\n[merged summary]", "user"); n != 0 {
			t.Fatalf("carrier rows %d in %v", n, childRows)
		}
		if n := countRows(childRows, "current ask\n\n[merged summary]\n\nhandoff scaffolding", ""); n != 1 {
			t.Fatalf("merged rows %d in %v", n, childRows)
		}
	})
}
