package state

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Fresh store with the timeline-root desktop session the hermes fixture creates
func timelineStore(t *testing.T) (*SessionDB, string) {
	t.Helper()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "timeline-root", Source: "desktop"})
	return db, "timeline-root"
}

// Default visibility message rows of a session
func timelineRows(t *testing.T, db *SessionDB, sid string) []*pyjson.Object {
	t.Helper()
	rows, err := db.GetMessages(context.Background(), sid, GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Timeline page of a session
func timelinePage(t *testing.T, db *SessionDB, sid string, limit int, after int64) *pyjson.Object {
	t.Helper()
	page, err := db.GetSessionTimeline(context.Background(), sid, limit, after)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

// Jump page anchored at a prompt row, failing when the anchor is refused
func aroundPage(t *testing.T, db *SessionDB, sid string, rowID int64, limit int) *pyjson.Object {
	t.Helper()
	page, err := db.GetSessionMessagesAround(context.Background(), sid, rowID, limit)
	if err != nil || page == nil {
		t.Fatalf("around %d: %v %v", rowID, page, err)
	}
	return page
}

// One pagination field of a page
func pageField(page *pyjson.Object, key string) any {
	pagination, _ := page.Get("pagination")
	v, _ := pagination.(*pyjson.Object).Get(key)
	return v
}

// Entry objects of a timeline page
func timelineEntries(page *pyjson.Object) []*pyjson.Object {
	raw, _ := page.Get("entries")
	out := []*pyjson.Object{}
	for _, e := range raw.([]any) {
		out = append(out, e.(*pyjson.Object))
	}
	return out
}

// Preview texts of a timeline page
func timelinePreviews(page *pyjson.Object) []string {
	out := []string{}
	for _, e := range timelineEntries(page) {
		out = append(out, pyStr(rowValue(e, "preview")))
	}
	return out
}

// Message maps of a jump page
func timelineMessages(page *pyjson.Object) []map[string]any {
	raw, _ := page.Get("messages")
	out := []map[string]any{}
	for _, m := range raw.([]any) {
		out = append(out, m.(*pyjson.Object).Vals)
	}
	return out
}

// Row id of an entry or message
func timelineID(t *testing.T, v any) int64 {
	t.Helper()
	id, ok := exactInt(v)
	if !ok {
		t.Fatalf("row id %v", v)
	}
	return id
}

func TestTimelinePagesProjectHumanPromptsWithoutToolPayloads(t *testing.T) {
	db, sid := timelineStore(t)
	archiveBatch(t, db, sid,
		map[string]any{"role": "user", "content": " First\n  prompt ", "timestamp": int64(200)},
		map[string]any{"role": "assistant", "content": "answer", "tool_calls": []any{map[string]any{
			"id": "tool-1", "function": map[string]any{"name": "terminal", "arguments": strings.Repeat("x", 10000)}}}},
		map[string]any{"role": "tool", "content": strings.Repeat("secret tool payload", 1000), "tool_call_id": "tool-1"},
		map[string]any{"role": "user", "content": strings.Repeat("é", 150), "timestamp": int64(100)})
	var expected []any
	for _, row := range timelineRows(t, db, sid) {
		if rowValue(row, "role") == "user" {
			expected = append(expected, rowValue(row, "id"))
		}
	}
	first := timelinePage(t, db, sid, 1, 0)
	entries, _ := first.Get("entries")
	want := []any{pyjson.ObjectOf("row_id", expected[0], "preview", "First prompt", "timestamp", int64(200))}
	if !pyjson.Equal(entries, want) {
		t.Fatalf("entries %v", pyjson.Plain(entries))
	}
	if !pyjson.Equal(pageField(first, "total"), int64(2)) || !pyjson.Equal(pageField(first, "returned"), int64(1)) || pageField(first, "has_more") != true {
		t.Fatalf("pagination %v", pyjson.Plain(pageField(first, "total")))
	}
	second := timelinePage(t, db, sid, 1, timelineID(t, pageField(first, "next_cursor")))
	rows := timelineEntries(second)
	if len(rows) != 1 || !pyjson.Equal(rowValue(rows[0], "row_id"), expected[1]) || pystr.Len(pyStr(rowValue(rows[0], "preview"))) != 120 {
		t.Fatalf("second page %v", pyjson.Plain(second))
	}
	if pageField(second, "has_more") != false || pageField(second, "next_cursor") != nil {
		t.Fatalf("second pagination %v", pyjson.Plain(pageField(second, "next_cursor")))
	}
	text, err := pyDumps(first, pyJSONDefault)
	if err != nil || strings.Contains(text, "secret tool payload") {
		t.Fatalf("tool payload leaked into %s %v", text, err)
	}
}

func TestCompactedTimelineAndJumpShareDisplayOrderAndVisibility(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "indexed"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, sid := timelineStore(t)
			archiveBatch(t, db, sid,
				map[string]any{"role": "user", "content": "old ask", "timestamp": int64(10)},
				map[string]any{"role": "assistant", "content": "old answer", "timestamp": int64(11)},
				map[string]any{"role": "user", "content": "retained ask", "timestamp": int64(12)},
				map[string]any{"role": "assistant", "content": "retained answer", "timestamp": int64(13)})
			compacted := []map[string]any{{"role": "user", "content": SummaryPrefix + "summary", "_compressed_summary": true}}
			for _, row := range timelineRows(t, db, sid)[2:] {
				compacted = append(compacted, row.Vals)
			}
			archiveCompact(t, db, sid, compacted, ArchiveCompactOptions{})
			archiveBatch(t, db, sid,
				map[string]any{"role": "user", "content": "hidden ask", "display_kind": "hidden"},
				map[string]any{"role": "user", "content": CompressionContinuationUserContent},
				map[string]any{"role": "user", "content": "[IMPORTANT: Background process 12 completed]"},
				map[string]any{"role": "user", "content": "[ASYNC DELEGATION BATCH COMPLETE] completed"},
				map[string]any{"role": "user", "content": "A background subagent you dispatched earlier has finished."},
				map[string]any{"role": "user", "content": SummaryPrefix + "handoff\n" + SummaryEndMarker + "\nlive ask",
					"_compressed_summary": true, "display_kind": "hidden"},
				map[string]any{"role": "assistant", "content": "live answer"},
				map[string]any{"role": "user", "content": "rewound ask"})
			rows := timelineRows(t, db, sid)
			rewoundID := timelineID(t, rowValue(rows[len(rows)-1], "id"))
			if err := db.writeSQL(ctx, "UPDATE messages SET active = 0, compacted = 0 WHERE id = ?", []any{rewoundID}, false, nil); err != nil {
				t.Fatal(err)
			}
			if legacy {
				if err := db.writeSQL(ctx, "UPDATE messages SET display_identity = NULL, display_order = NULL WHERE session_id = ?", []any{sid}, false, nil); err != nil {
					t.Fatal(err)
				}
			}
			timeline := timelinePage(t, db, sid, 500, 0)
			if got := timelinePreviews(timeline); !reflect.DeepEqual(got, []string{"old ask", "retained ask", "live ask"}) {
				t.Fatalf("previews %v", got)
			}
			entries := timelineEntries(timeline)
			jump := aroundPage(t, db, sid, timelineID(t, rowValue(entries[1], "row_id")), 2)
			var contents []any
			for _, m := range timelineMessages(jump) {
				contents = append(contents, m["content"])
			}
			if !reflect.DeepEqual(contents, []any{"retained ask", "retained answer"}) || pageField(jump, "has_older") != true || pageField(jump, "has_newer") != true {
				t.Fatalf("jump %v %v", contents, pyjson.Plain(pageField(jump, "has_older")))
			}
			last := aroundPage(t, db, sid, timelineID(t, rowValue(entries[len(entries)-1], "row_id")), 120)
			msgs := timelineMessages(last)
			if len(msgs) != 2 || ProjectCompactionMessageForDisplay(msgs[0])["content"] != "live ask" || msgs[1]["content"] != "live answer" || pageField(last, "has_newer") != false {
				t.Fatalf("last page %v", pyjson.Plain(last))
			}
			if page, err := db.GetSessionMessagesAround(ctx, sid, rewoundID, 120); err != nil || page != nil {
				t.Fatalf("rewound row answered %v %v", pyjson.Plain(page), err)
			}
		})
	}
}

func TestCursorSurvivesCompactionBetweenPages(t *testing.T) {
	db, sid := timelineStore(t)
	var batch []map[string]any
	for i := range 4 {
		batch = append(batch, map[string]any{"role": "user", "content": fmt.Sprintf("ask %d", i), "timestamp": int64(100 - i)})
	}
	archiveBatch(t, db, sid, batch...)
	first := timelinePage(t, db, sid, 2, 0)
	firstEntries := timelineEntries(first)
	oldRowID := timelineID(t, rowValue(firstEntries[len(firstEntries)-1], "row_id"))
	var retained []map[string]any
	for _, row := range timelineRows(t, db, sid)[1:] {
		retained = append(retained, row.Vals)
	}
	archiveCompact(t, db, sid, retained, ArchiveCompactOptions{})
	second := timelinePage(t, db, sid, 2, timelineID(t, pageField(first, "next_cursor")))
	if got := append(timelinePreviews(first), timelinePreviews(second)...); !reflect.DeepEqual(got, []string{"ask 0", "ask 1", "ask 2", "ask 3"}) {
		t.Fatalf("previews %v", got)
	}
	if !pyjson.Equal(pageField(second, "total"), int64(4)) || pageField(second, "has_more") != false {
		t.Fatalf("second pagination %v", pyjson.Plain(second))
	}
	current := timelineEntries(timelinePage(t, db, sid, 500, 0))
	if timelineID(t, rowValue(current[1], "row_id")) <= oldRowID {
		t.Fatalf("compacted prompt kept its old row id %v", pyjson.Plain(current[1]))
	}
	empty := timelinePage(t, db, sid, 500, 99999)
	if len(timelineEntries(empty)) != 0 || !pyjson.Equal(pageField(empty, "total"), int64(4)) || pageField(empty, "has_more") != false {
		t.Fatalf("empty page %v", pyjson.Plain(empty))
	}
}

func TestTimelineSqlNeverReadsToolColumnsOrWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db := usageOpen(t, path)
	readSession(t, db, NewSession{ID: "timeline-root", Source: "desktop"})
	archiveBatch(t, db, "timeline-root",
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "multimodal ask"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:unused"}}}},
		map[string]any{"role": "tool", "content": "unread tool result", "tool_calls": []any{map[string]any{"unused": "payload"}}})
	reader := mustSessionDB(t, path, true)
	defer reader.Close()
	var accesses [][2]string
	denied := map[string]bool{"tool_calls": true, "reasoning": true, "api_content": true, "codex_reasoning_items": true}
	authorize := func(action int, table, column, _, _ string) int {
		if action == pysqlite.ActionRead {
			accesses = append(accesses, [2]string{table, column})
			if table == "messages" && denied[column] {
				return pysqlite.AuthDeny
			}
		}
		if action == pysqlite.ActionUpdate || action == pysqlite.ActionInsert || action == pysqlite.ActionDelete {
			return pysqlite.AuthDeny
		}
		return pysqlite.AuthOK
	}
	err := reader.readCtx(ctx, true, func(conn SQLiteConn) error {
		raw, ok := rawConnOf(conn)
		if !ok {
			t.Fatalf("read connection %T exposes no raw handle", conn)
		}
		if err := pysqlite.SetAuthorizer(raw, authorize); err != nil {
			return err
		}
		defer pysqlite.SetAuthorizer(raw, nil)
		page, err := reader.GetSessionTimeline(withReadConn(ctx, conn), "timeline-root", 500, 0)
		if err != nil {
			return err
		}
		entries := timelineEntries(page)
		if len(entries) != 1 || rowValue(entries[0], "preview") != "multimodal ask" || !pyjson.Equal(pageField(page, "total"), int64(1)) || len(accesses) == 0 {
			t.Fatalf("guarded page %v accesses %d", pyjson.Plain(page), len(accesses))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRealMessageThatMentionsMarkerTextIsUntouched(t *testing.T) {
	message := map[string]any{"id": int64(1), "session_id": "s1", "role": "user",
		"content":    "please explain the string [CONTEXT COMPACTION] in this bug report",
		"tool_calls": []any{map[string]any{"id": "real"}}, "reasoning": "real provider payload"}
	projected := ProjectCompactionMessageForDisplay(message)
	if !reflect.DeepEqual(projected, message) {
		t.Fatalf("projection changed the message: %v", projected)
	}
	projected["content"] = "changed"
	if message["content"] == "changed" {
		t.Fatal("projection returned the same dict")
	}
}
