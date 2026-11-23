package tuiserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Marks the session running with turn A in flight
func queuedPromptBusy(session *tuihost.Session) {
	unlock := lockHistory(session)
	session.Set("running", true)
	startInflightTurn(session, "prompt A", "", nil)
	unlock()
}

// Turn A concludes and the session goes idle
func queuedPromptConcludeA(t *testing.T, db *state.SessionDB, session *tuihost.Session, key string) {
	t.Helper()
	if _, err := db.AppendMessage(context.Background(), key, map[string]any{"role": "assistant", "content": "reply A"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	unlock := lockHistory(session)
	session.Set("running", false)
	clearInflightTurn(session)
	unlock()
}

// Accepts a prompt while the session is busy and requires it queued
func queuedPromptAccept(t *testing.T, sid string, session *tuihost.Session, rid, text string) {
	t.Helper()
	resp := handleBusySubmit(context.Background(), rid, sid, session, text, "ws-1", true, nil, "")
	if result, _ := resp["result"].(map[string]any); result["status"] != "queued" {
		t.Fatalf("busy submit %v", resp)
	}
}

// Replaces the submit runner with the turn body that adopts the staged row, crash persists it and flushes the reply
func queuedPromptRunTurns(t *testing.T, db *state.SessionDB, key, reply string) {
	t.Helper()
	saved := runPromptSubmit
	t.Cleanup(func() { runPromptSubmit = saved })
	runPromptSubmit = func(ctx context.Context, _ any, _ string, session *tuihost.Session, text any, _ promptSubmitOptions) bool {
		a := submitRowFlushAgent(t, db, key)
		adoptSubmitUserRow(ctx, session, a, text, text)
		userMsg, _ := a.StageTurnUserMessage(text, text)
		messages := []map[string]any{userMsg}
		a.SetPersistUserMessageIdx(0)
		if outcome := a.FlushMessagesToSessionDB(messages); outcome == state.FlushFailed {
			t.Fatalf("flush outcome %v", outcome)
		}
		if outcome := a.FlushMessagesToSessionDB(append(messages, map[string]any{"role": "assistant", "content": reply})); outcome == state.FlushFailed {
			t.Fatalf("flush outcome %v", outcome)
		}
		return false
	}
}

// Active repaired rows of the stored session with their row ids
func queuedPromptActiveRows(t *testing.T, db *state.SessionDB, key string) []map[string]any {
	t.Helper()
	rows, err := db.GetMessagesAsConversation(context.Background(), key, state.ConversationOptions{RepairAlternation: true, IncludeRowIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// The live turn compacts in place over its own rows, never the queued prompt's accept time row
func queuedPromptCompactInPlace(t *testing.T, db *state.SessionDB, key string) {
	t.Helper()
	ctx := context.Background()
	rows, err := db.GetMessagesAsConversation(ctx, key, state.ConversationOptions{IncludeRowIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	var held []int64
	for _, r := range rows {
		if !strings.Contains(fmt.Sprint(r["content"]), "QUEUED-MARKER") {
			held = append(held, r["_row_id"].(int64))
		}
	}
	compacted := []map[string]any{{"role": "user", "content": "[CONTEXT COMPACTION] summary of turn A"}, {"role": "assistant", "content": "ok, continuing"}}
	if _, err := db.ArchiveAndCompact(ctx, key, compacted, state.ArchiveCompactOptions{CoveredIDs: held, UnresolvedHeld: []map[string]any{}}); err != nil {
		t.Fatal(err)
	}
}
