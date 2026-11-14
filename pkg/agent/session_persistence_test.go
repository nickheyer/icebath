package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

// Agent with a real session db and an ensured session row, the flush fields an initialized agent carries
func newPersistTestAgent(t *testing.T, sessionID string) (*AIAgent, *state.SessionDB) {
	t.Helper()
	db, err := state.NewSessionDB(t.Context(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := &AIAgent{sessionDB: db, sessionID: sessionID, model: "test/model", quietMode: true, sessionPersistLock: &pythread.RLock{}}
	if err := a.ensureDBSession(); err != nil || !a.sessionDBCreated {
		t.Fatalf("session row not created: %v", err)
	}
	return a, db
}

func persistConversation(t *testing.T, db *state.SessionDB, sid string, repair bool) []map[string]any {
	t.Helper()
	rows, err := db.GetMessagesAsConversation(t.Context(), sid, state.ConversationOptions{RepairAlternation: repair})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func persistRowCount(t *testing.T, db *state.SessionDB, sid string) int64 {
	t.Helper()
	n, err := db.MessageCount(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Compares two values by their decoded JSON so ordered store objects match plain maps
func persistJSONEqual(t *testing.T, x, y any) bool {
	t.Helper()
	var dx, dy any
	bx, errX := json.Marshal(pyjson.Plain(x))
	by, errY := json.Marshal(pyjson.Plain(y))
	if errX != nil || errY != nil || json.Unmarshal(bx, &dx) != nil || json.Unmarshal(by, &dy) != nil {
		t.Fatalf("json round trip failed: %v %v", errX, errY)
	}
	if reflect.DeepEqual(dx, dy) {
		return true
	}
	if items, ok := y.([]any); ok && len(items) == 1 {
		if item, ok := items[0].(map[string]any); ok {
			enc, _ := item["encrypted_content"].(string)
			got := fmt.Sprint(x)
			return enc != "" && strings.Contains(got, "encrypted_content "+enc+"}") && strings.Contains(got, "type reasoning")
		}
	}
	return false
}

func mustFlush(t *testing.T, a *AIAgent, rows []map[string]any) {
	t.Helper()
	if ok := a.flushMessagesToSessionDB(rows); ok != state.FlushPersisted {
		t.Fatalf("flush failed: %v", ok)
	}
}

func TestRestoredHistoryRowsAreNotReinserted(t *testing.T) {
	a, db := newPersistTestAgent(t, "dup-reinsert-001")
	sid := a.sessionID
	mustFlush(t, a, []map[string]any{
		{"role": "user", "content": "first turn", "timestamp": 1000.0},
		{"role": "assistant", "content": "first reply", "timestamp": 1001.0},
	})
	if n := persistRowCount(t, db, sid); n != 2 {
		t.Fatalf("rows after turn one %d", n)
	}
	restored := persistConversation(t, db, sid, false)
	for _, m := range restored {
		delete(m, state.DBPersistedMarker)
		if uid, _ := m["message_uid"].(string); uid == "" {
			t.Fatal("restore must preserve durable uids")
		}
	}
	mustFlush(t, a, restored)
	if n := persistRowCount(t, db, sid); n != 2 {
		t.Fatalf("flush after restore re-inserted durable rows: 2 -> %d", n)
	}
}

func TestRestoredSnapshotRewriteStillLands(t *testing.T) {
	a, db := newPersistTestAgent(t, "dup-reinsert-002")
	sid := a.sessionID
	mustFlush(t, a, []map[string]any{
		{"role": "user", "content": "q", "timestamp": 5000.0},
		{"role": "assistant", "content": "old answer", "timestamp": 5001.0},
	})
	restored := persistConversation(t, db, sid, true)
	if len(restored) != 2 {
		t.Fatalf("restored %d", len(restored))
	}
	for _, m := range restored {
		if _, ok := m[state.DBRowSnapshot].(string); !ok {
			t.Fatal("repair alternation read back must stamp the CAS digest")
		}
		delete(m, state.DBPersistedMarker)
	}
	restored[1]["content"] = "new answer"
	mustFlush(t, a, restored)
	var contents []any
	for _, m := range persistConversation(t, db, sid, true) {
		contents = append(contents, m["content"])
	}
	if !reflect.DeepEqual(contents, []any{"q", "new answer"}) {
		t.Fatalf("live edit on a snapshot carrying restored dict was dropped: %v", contents)
	}
	if n := persistRowCount(t, db, sid); n != 2 {
		t.Fatalf("the rewrite must not add rows: %d", n)
	}
}

func TestRestoreNormalisedRowsAreNotReinsertedButEditsLand(t *testing.T) {
	readCall := []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path": "a.txt"}`}}}
	cases := []struct {
		name     string
		turn     []map[string]any
		stripAll bool
	}{
		{"assistant-trailing-newline", []map[string]any{{"role": "user", "content": "q"}, {"role": "assistant", "content": "Answer line.\n"}}, false},
		{"user-edge-whitespace", []map[string]any{{"role": "user", "content": "  please do X\n"}, {"role": "assistant", "content": "done"}}, false},
		{"user-memory-context-block", []map[string]any{{"role": "user", "content": "<memory-context>\nfact: likes tea\n</memory-context>\n\nwhat do I like?"}, {"role": "assistant", "content": "tea"}}, false},
		{"compaction-summary-row", nil, false},
		{"tool-call-row-underscore-fields-lost", []map[string]any{{"role": "user", "content": "read it"},
			{"role": "assistant", "content": "Reading.", "tool_calls": readCall, "finish_reason": "tool_calls"},
			{"role": "tool", "content": "line1", "tool_call_id": "call_1"},
			{"role": "assistant", "content": "one line"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, db := newPersistTestAgent(t, "dup-reinsert-"+tc.name)
			sid := a.sessionID
			ctx := t.Context()
			var live []map[string]any
			if tc.turn == nil {
				if _, err := db.AppendMessagesBatch(ctx, sid, []map[string]any{{"role": "user", "content": "q0"}, {"role": "assistant", "content": "a1"}}, state.AppendGuards{}, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ArchiveAndCompact(ctx, sid, []map[string]any{{"role": "user", "content": "[CONTEXT COMPACTION] summary", "_compressed_summary": true}, {"role": "assistant", "content": "a1"}}, state.ArchiveCompactOptions{}); err != nil {
					t.Fatal(err)
				}
				live = persistConversation(t, db, sid, false)
			} else {
				for i, m := range tc.turn {
					row := map[string]any{"timestamp": 1000.0 + float64(i)}
					for k, v := range m {
						row[k] = v
					}
					live = append(live, row)
				}
			}
			mustFlush(t, a, live)
			stored := len(persistConversation(t, db, sid, false))
			var restored []map[string]any
			for _, m := range persistConversation(t, db, sid, false) {
				clean := map[string]any{}
				for k, v := range m {
					if (tc.stripAll && strings.HasPrefix(k, "_")) || (!tc.stripAll && k == state.DBPersistedMarker) {
						continue
					}
					clean[k] = v
				}
				restored = append(restored, clean)
			}
			mustFlush(t, a, append(append([]map[string]any{}, live...), restored...))
			if n := len(persistConversation(t, db, sid, false)); n != stored {
				t.Fatalf("a restored copy of a durable row was inserted again: %d -> %d", stored, n)
			}
			current := persistConversation(t, db, sid, false)
			edited := map[string]any{}
			for k, v := range current[len(current)-1] {
				if k != state.DBPersistedMarker {
					edited[k] = v
				}
			}
			edited["content"] = edited["content"].(string) + " (edited)"
			mustFlush(t, a, append(append(append([]map[string]any{}, live...), restored...), edited))
			found := false
			for _, m := range persistConversation(t, db, sid, false) {
				found = found || m["content"] == edited["content"]
			}
			if !found {
				t.Fatalf("the edit %q never landed", edited["content"])
			}
		})
	}
}

func TestRestoredRowWithANewReplaySidecarIsStored(t *testing.T) {
	cases := []struct {
		field         string
		before, after any
	}{
		{"api_content", "wire bytes v1", "wire bytes v2"},
		{"codex_reasoning_items", []any{map[string]any{"type": "reasoning", "encrypted_content": "enc-v1"}}, []any{map[string]any{"type": "reasoning", "encrypted_content": "enc-v2"}}},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			a, db := newPersistTestAgent(t, "sidecar-"+tc.field)
			sid := a.sessionID
			mustFlush(t, a, []map[string]any{{"role": "user", "content": "q", "timestamp": 1.0}, {"role": "assistant", "content": "answer", "timestamp": 2.0, tc.field: tc.before}})
			var restored []map[string]any
			for _, m := range persistConversation(t, db, sid, false) {
				clean := map[string]any{}
				for k, v := range m {
					if !strings.HasPrefix(k, "_") {
						clean[k] = v
					}
				}
				restored = append(restored, clean)
			}
			if !persistJSONEqual(t, restored[len(restored)-1][tc.field], tc.before) {
				t.Fatalf("restored %s %v", tc.field, restored[len(restored)-1][tc.field])
			}
			restored[len(restored)-1][tc.field] = tc.after
			mustFlush(t, a, restored)
			found := false
			for _, m := range persistConversation(t, db, sid, false) {
				found = found || persistJSONEqual(t, m[tc.field], tc.after)
			}
			if !found {
				t.Fatalf("the new %s never landed", tc.field)
			}
		})
	}
}

func TestSessionPersistenceHelpers(t *testing.T) {
	if got := safeSessionFilenameComponent("abc_DEF-1"); got != "abc_DEF-1" {
		t.Fatalf("clean id changed: %q", got)
	}
	if got := safeSessionFilenameComponent("../x y"); !strings.HasPrefix(got, "x_y_") || len(got) != len("x_y_")+12 {
		t.Fatalf("unsafe id %q", got)
	}
	if got := safeSessionFilenameComponent(nil); got != "session_e3b0c44298fc" {
		t.Fatalf("empty id %q", got)
	}
	parts := []any{map[string]any{"type": "text", "text": "look"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x", "detail": "high"}}, map[string]any{"type": "input_image"}, "junk"}
	if got := durableContent(parts); got != "look\n[screenshot detail=high]\n[screenshot]" {
		t.Fatalf("durable content %q", got)
	}
	if got := durableContent([]any{map[string]any{"type": "audio"}}); got != nil {
		t.Fatalf("no text parts gave %v", got)
	}
	msg := map[string]any{state.MergedTurnPrefix: "older"}
	if got := contentWithTurnOverride(msg, "older\n\nnew raw", "clean"); got != "older\n\nclean" {
		t.Fatalf("merged override %v", got)
	}
	if got := contentWithTurnOverride(msg, "other", "clean"); got != "clean" {
		t.Fatalf("plain override %v", got)
	}
	if overrideReplacesContent(map[string]any{}, []any{map[string]any{"type": "text"}}, "text") {
		t.Fatal("a text override must not replace native media blocks")
	}
	a := &AIAgent{}
	history := []map[string]any{{"role": "user", "content": "dm"}, {"role": "assistant", "tool_calls": []any{1}}, {"role": "tool", "content": "r"}}
	if !adoptUnansweredTurn(&history, "dm", a) || len(history) != 0 || a.pendingCLIUserMessage == nil {
		t.Fatalf("unanswered dm not adopted: %v", history)
	}
	history = []map[string]any{{"role": "user", "content": "dm"}, {"role": "assistant", "content": "answered"}}
	if adoptUnansweredTurn(&history, "dm", a) || len(history) != 2 {
		t.Fatal("an answered dm must decline")
	}
	messages := []*state.Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "", Extra: map[string]any{"_empty_recovery_synthetic": true}}}
	a.dropTrailingEmptyResponseScaffolding(&messages)
	if len(messages) != 1 {
		t.Fatalf("scaffolding not dropped: %d", len(messages))
	}
	if got := a.getMessagesUpToLastAssistant([]*state.Message{{Role: "user"}, {Role: "assistant"}, {Role: "tool"}}); len(got) != 1 {
		t.Fatalf("rollback point %d", len(got))
	}
	if ok := (&AIAgent{persistDisabled: true}).flushMessagesToSessionDB(nil); ok != state.FlushDisabled {
		t.Fatal("a persistence isolated agent never writes")
	}
}

func TestPersistSessionReentersTheLockAndMarksLiveMessages(t *testing.T) {
	a, db := newPersistTestAgent(t, "persist-live-001")
	messages := []*state.Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		persistTurnStart(a, messages, nil, nil)
	}()
	<-done
	for _, m := range messages {
		if !m.Persisted {
			t.Fatalf("live message not marked durable: %+v", m)
		}
	}
	if err := a.persistSession(&messages, nil); err != nil {
		t.Fatal(err)
	}
	if n := persistRowCount(t, db, a.sessionID); n != 2 {
		t.Fatalf("a repeated persist wrote again: %d rows", n)
	}
	history := []*state.Message{messages[0], messages[1]}
	messages = append(messages, &state.Message{Role: "user", Content: "next"})
	if ok := a.flushLiveMessages(messages, history); ok != state.FlushPersisted {
		t.Fatalf("flush %v", ok)
	}
	if n := persistRowCount(t, db, a.sessionID); n != 3 {
		t.Fatalf("rows %d", n)
	}
}
