package tuiserver

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Desktop session over a fresh store with its lazy row written and turn A's prompt in the transcript
func queuedPromptSession(t *testing.T) (*state.SessionDB, string, *tuigateway.Session, string) {
	t.Helper()
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
	ensureSessionDBRow(ctx, session)
	if _, err := db.AppendMessage(ctx, key, map[string]any{"role": "user", "content": "prompt A"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	return db, sid, session, key
}

// Marks the session running with turn A in flight
func queuedPromptBusy(session *tuigateway.Session) {
	unlock := lockHistory(session)
	session.Set("running", true)
	startInflightTurn(session, "prompt A", "", nil)
	unlock()
}

// Turn A concludes and the session goes idle
func queuedPromptConcludeA(t *testing.T, db *state.SessionDB, session *tuigateway.Session, key string) {
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
func queuedPromptAccept(t *testing.T, sid string, session *tuigateway.Session, rid, text string) {
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
	runPromptSubmit = func(ctx context.Context, _ any, _ string, session *tuigateway.Session, text any, _ promptSubmitOptions) bool {
		a := submitRowFlushAgent(t, db, key)
		adoptSubmitUserRow(ctx, session, a, text, text)
		userMsg, _ := a.StageTurnUserMessage(text, text)
		messages := []map[string]any{userMsg}
		a.SetPersistUserMessageIdx(0)
		if _, err := a.FlushMessagesToSessionDB(messages); err != nil {
			t.Fatal(err)
		}
		if _, err := a.FlushMessagesToSessionDB(append(messages, map[string]any{"role": "assistant", "content": reply})); err != nil {
			t.Fatal(err)
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

func TestQueuedPromptPersistence(t *testing.T) {
	t.Run("test_partial_drain_never_puts_a_later_prompt_before_an_earlier_one", func(t *testing.T) {
		db, sid, session, key := queuedPromptSession(t)
		queuedPromptBusy(session)
		session.Set("attached_images", []string{"/tmp/b.png"})
		queuedPromptAccept(t, sid, session, "r1", "prompt B QUEUED-B")
		session.Set("attached_images", []string{"/tmp/c.png"})
		queuedPromptAccept(t, sid, session, "r2", "prompt C QUEUED-C")
		queuedPromptConcludeA(t, db, session, key)
		queuedPromptRunTurns(t, db, key, "reply B")
		if !drainQueuedPrompt(context.Background(), "r3", sid, session) {
			t.Fatal("the first queued prompt did not drain")
		}
		session.Set("queued_prompt", nil)
		session.Pop("queued_prompts")
		var rendered strings.Builder
		for _, r := range queuedPromptActiveRows(t, db, key) {
			rendered.WriteString(fmt.Sprint(r["content"]))
		}
		text := rendered.String()
		b, c := strings.Index(text, "prompt B QUEUED-B"), strings.Index(text, "prompt C QUEUED-C")
		if b < 0 || c < 0 {
			t.Fatalf("both queued prompts must stay durable: %q", text)
		}
		if b >= c {
			t.Fatalf("a later-accepted prompt rendered before an earlier one after a partial drain: %q", text)
		}
	})
	t.Run("test_drain_after_in_place_compaction_leaves_one_active_queued_row", func(t *testing.T) {
		db, sid, session, key := queuedPromptSession(t)
		queuedPromptBusy(session)
		handleBusySubmit(context.Background(), "r1", sid, session, "queued text QUEUED-MARKER", "ws-1", true, nil, "")
		queuedPromptCompactInPlace(t, db, key)
		queuedPromptConcludeA(t, db, session, key)
		queuedPromptRunTurns(t, db, key, "reply B")
		if !drainQueuedPrompt(context.Background(), "r2", sid, session) {
			t.Fatal("the queued prompt did not drain")
		}
		var rows [][2]any
		count := 0
		for _, r := range queuedPromptActiveRows(t, db, key) {
			rows = append(rows, [2]any{r["role"], r["content"]})
			if r["content"] == "queued text QUEUED-MARKER" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("queued prompt active %d times in %v", count, rows)
		}
		n := len(rows)
		if n < 3 || !reflect.DeepEqual(rows[n-2:], [][2]any{{"user", "queued text QUEUED-MARKER"}, {"assistant", "reply B"}}) {
			t.Fatalf("tail %v", rows)
		}
		if content, _ := rows[n-3][1].(string); rows[n-3][0] != "assistant" || !strings.HasSuffix(content, "reply A") {
			t.Fatalf("reply A must precede the queued prompt: %v", rows)
		}
	})
}
