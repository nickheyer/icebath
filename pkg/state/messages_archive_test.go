package state

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const archiveSummary = "[CONTEXT COMPACTION] summary of the earlier turns"

func archiveSummaryMsg() map[string]any {
	return map[string]any{"role": "user", "content": archiveSummary}
}

func archiveDB(t *testing.T) (*SessionDB, string) {
	t.Helper()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "sess1", Source: "test"})
	for i := range 4 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		readAppend(t, db, "sess1", map[string]any{"role": role, "content": fmt.Sprintf("old turn %d", i)})
	}
	return db, "sess1"
}

type archiveRow struct {
	id                int64
	active, compacted int64
	content, uid      any
}

func archiveRows(t *testing.T, db *SessionDB, sid string) []archiveRow {
	t.Helper()
	rows, err := db.readAll(context.Background(), "SELECT id, active, compacted, content, message_uid FROM messages WHERE session_id = ? ORDER BY id", sid)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]archiveRow, len(rows))
	for i, r := range rows {
		out[i] = archiveRow{r.Vals["id"].(int64), r.Vals["active"].(int64), r.Vals["compacted"].(int64), r.Vals["content"], r.Vals["message_uid"]}
	}
	return out
}

func archiveCopies(t *testing.T, db *SessionDB, sid, content string) []archiveRow {
	var out []archiveRow
	for _, r := range archiveRows(t, db, sid) {
		if r.content == content {
			out = append(out, r)
		}
	}
	return out
}

func archiveConversation(t *testing.T, db *SessionDB, sid string, rowIDs bool) []map[string]any {
	t.Helper()
	msgs, err := db.GetMessagesAsConversation(context.Background(), sid, ConversationOptions{IncludeRowIDs: rowIDs})
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func archiveContents(msgs []map[string]any) []any {
	out := make([]any, len(msgs))
	for i, m := range msgs {
		out[i] = m["content"]
	}
	return out
}

func archiveDisplayContents(t *testing.T, db *SessionDB, sid string) []any {
	t.Helper()
	rows, err := db.GetMessages(context.Background(), sid, GetMessagesOptions{IncludeCompacted: true})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r.Vals["content"]
	}
	return out
}

func archiveCompact(t *testing.T, db *SessionDB, sid string, compacted []map[string]any, o ArchiveCompactOptions) int {
	t.Helper()
	n, err := db.ArchiveAndCompact(context.Background(), sid, compacted, o)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func archiveWatermark(t *testing.T, db *SessionDB, sid string) *int64 {
	t.Helper()
	w, err := db.GetActiveMessageWatermark(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return &w
}

func archiveBatch(t *testing.T, db *SessionDB, sid string, msgs ...map[string]any) {
	t.Helper()
	if _, err := db.AppendMessagesBatch(context.Background(), sid, msgs, AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
}

func countOf(items []any, want any) int {
	n := 0
	for _, it := range items {
		if it == want {
			n++
		}
	}
	return n
}

func TestHeldRowAboveWatermarkIsCarriedOnce(t *testing.T) {
	db, sid := archiveDB(t)
	watermark := archiveWatermark(t, db, sid)
	readAppend(t, db, sid, map[string]any{"role": "user", "content": "steer during the summary call"})
	readAppend(t, db, sid, map[string]any{"role": "assistant", "content": "reply during the summary call"})
	conv := archiveConversation(t, db, sid, true)
	held := conv[len(conv)-2:]
	for _, m := range held {
		if !pyTruthy(m["_row_id"]) || !pyTruthy(m["message_uid"]) {
			t.Fatalf("held dict lacks its names: %v", m)
		}
	}
	if n := archiveCompact(t, db, sid, append([]map[string]any{archiveSummaryMsg()}, held...), ArchiveCompactOptions{Watermark: watermark, TailCount: 2}); n != 3 {
		t.Fatalf("count %d", n)
	}
	want := []any{archiveSummary, "steer during the summary call", "reply during the summary call"}
	if got := archiveContents(archiveConversation(t, db, sid, false)); !slices.Equal(got, want) {
		t.Fatalf("live %v", got)
	}
	for _, content := range want[1:] {
		copies := archiveCopies(t, db, sid, content.(string))
		if len(copies) != 2 || copies[0].active+copies[1].active != 1 || copies[0].uid != copies[1].uid {
			t.Fatalf("%v: original plus carried copy expected, got %+v", content, copies)
		}
		hits, err := db.sessionSearch.SearchMessages(context.Background(), content.(string), SearchMessagesOptions{Limit: 20})
		if err != nil || len(hits) != 1 {
			t.Fatalf("search %v: %d hits %v", content, len(hits), err)
		}
	}
	for i := range 4 {
		copies := archiveCopies(t, db, sid, fmt.Sprintf("old turn %d", i))
		if len(copies) != 1 || copies[0].active != 0 || copies[0].compacted != 1 {
			t.Fatalf("old turn %d: %+v", i, copies)
		}
	}
}

func TestForeignIdenticalRowAboveWatermarkIsStillCloned(t *testing.T) {
	db, sid := archiveDB(t)
	readAppend(t, db, sid, map[string]any{"role": "assistant", "content": "same text twice"})
	conv := archiveConversation(t, db, sid, true)
	held := conv[len(conv)-1]
	watermark := archiveWatermark(t, db, sid)
	readAppend(t, db, sid, map[string]any{"role": "assistant", "content": "same text twice"})
	archiveCompact(t, db, sid, []map[string]any{archiveSummaryMsg(), held}, ArchiveCompactOptions{Watermark: watermark, TailCount: 1})
	copies := archiveCopies(t, db, sid, "same text twice")
	uids := map[any]bool{}
	active := 0
	for _, c := range copies {
		if c.active == 1 {
			active++
			uids[c.uid] = true
		}
	}
	if len(copies) != 4 || active != 2 || len(uids) != 2 {
		t.Fatalf("held original plus copy and foreign original plus clone expected: %+v", copies)
	}
}

func TestCollidingStoredRowIsNeverDropped(t *testing.T) {
	db, sid := archiveDB(t)
	archiveBatch(t, db, sid, map[string]any{"role": "user", "content": "ok", "timestamp": 1000.0}, map[string]any{"role": "assistant", "content": "draft answer", "timestamp": 1001.0})
	conv := archiveConversation(t, db, sid, true)
	heldOK, heldDraft := conv[len(conv)-2], conv[len(conv)-1]
	watermark := archiveWatermark(t, db, sid)
	archiveBatch(t, db, sid,
		map[string]any{"role": "user", "content": "ok", "timestamp": 1000.0},
		map[string]any{"role": "assistant", "content": "edited answer", "timestamp": 1002.0, "message_uid": heldDraft["message_uid"]},
		map[string]any{"role": "user", "content": "continue", "timestamp": 1003.0})
	archiveCompact(t, db, sid, []map[string]any{archiveSummaryMsg(), heldOK, heldDraft, {"role": "user", "content": "continue"}}, ArchiveCompactOptions{Watermark: watermark, TailCount: 3})
	live := archiveContents(archiveConversation(t, db, sid, false))
	if countOf(live, "ok") != 2 || countOf(live, "edited answer") != 1 {
		t.Fatalf("live %v", live)
	}
	if n := countOf(archiveDisplayContents(t, db, sid), "continue"); n != 2 {
		t.Fatalf("display continue count %d", n)
	}
	conv = archiveConversation(t, db, sid, false)
	restored := conv[len(conv)-1]
	restored["content"] = "continue, and also this"
	restored[RestoredFromStore] = true
	delete(restored, DBPersistedMarker)
	archiveBatch(t, db, sid, restored)
	if countOf(archiveContents(archiveConversation(t, db, sid, false)), "continue, and also this") != 1 {
		t.Fatal("restored edit lost")
	}
}

func setUserContent(t *testing.T, db *SessionDB, sid string, rowID any, content string) {
	t.Helper()
	id, _ := pyIntID(rowID)
	if _, err := db.SetUserMessageContent(context.Background(), sid, id, content, nil); err != nil {
		t.Fatal(err)
	}
}

func shortenedAfterFlushedHold(before, after string, drop ...string) func(*testing.T, *SessionDB, string) ([]map[string]any, []any) {
	return func(t *testing.T, db *SessionDB, sid string) ([]map[string]any, []any) {
		flushed := map[string]any{"role": "user", "content": before}
		archiveBatch(t, db, sid, flushed)
		held := map[string]any{}
		for k, v := range flushed {
			if !slices.Contains(drop, k) {
				held[k] = v
			}
		}
		setUserContent(t, db, sid, flushed["_row_id"], after)
		return []map[string]any{archiveSummaryMsg(), held}, []any{archiveSummary, before, after}
	}
}

func TestRewrittenCarryClaimsOnlyWhatTheCompressorRewrote(t *testing.T) {
	cases := map[string]func(*testing.T, *SessionDB, string) ([]map[string]any, []any){
		"row_rewritten_after_hold": func(t *testing.T, db *SessionDB, sid string) ([]map[string]any, []any) {
			readAppend(t, db, sid, map[string]any{"role": "user", "content": "raw @file"})
			conv := archiveConversation(t, db, sid, true)
			row := conv[len(conv)-1]
			setUserContent(t, db, sid, row["_row_id"], "expanded file body")
			held := map[string]any{"role": "user", "content": "raw @file", "message_uid": row["message_uid"]}
			return []map[string]any{archiveSummaryMsg(), held}, []any{archiveSummary, "raw @file", "expanded file body"}
		},
		"row_shortened_after_row_addressed_load": func(t *testing.T, db *SessionDB, sid string) ([]map[string]any, []any) {
			readAppend(t, db, sid, map[string]any{"role": "user", "content": "fix bug\nthen delete database"})
			conv := archiveConversation(t, db, sid, true)
			held := conv[len(conv)-1]
			setUserContent(t, db, sid, held["_row_id"], "fix bug")
			return []map[string]any{archiveSummaryMsg(), held}, []any{archiveSummary, "fix bug\nthen delete database", "fix bug"}
		},
		"edit_reinserted_with_its_uid": func(t *testing.T, db *SessionDB, sid string) ([]map[string]any, []any) {
			readAppend(t, db, sid, map[string]any{"role": "assistant", "content": "v1 draft"})
			conv := archiveConversation(t, db, sid, false)
			uid := conv[len(conv)-1]["message_uid"]
			archiveBatch(t, db, sid, map[string]any{"role": "assistant", "content": "v2 edited", "message_uid": uid})
			held := map[string]any{"role": "assistant", "content": "v2 edited", "message_uid": uid}
			return []map[string]any{archiveSummaryMsg(), held}, []any{archiveSummary, "v2 edited", "v1 draft"}
		},
		"summary_merged_into_named_row": func(t *testing.T, db *SessionDB, sid string) ([]map[string]any, []any) {
			readAppend(t, db, sid, map[string]any{"role": "user", "content": "steer during the summary call"})
			conv := archiveConversation(t, db, sid, true)
			row := conv[len(conv)-1]
			merged := "[prior]\n" + row["content"].(string) + "\n\n" + archiveSummary
			held := map[string]any{}
			for k, v := range row {
				held[k] = v
			}
			held["content"], held["_compressed_summary"] = merged, true
			return []map[string]any{held}, []any{merged, row["content"]}
		},
		"summary_merged_into_flushed_row": func(t *testing.T, db *SessionDB, sid string) ([]map[string]any, []any) {
			flushed := map[string]any{"role": "user", "content": "  steer during the summary call"}
			archiveBatch(t, db, sid, flushed)
			merged := "[prior]\n" + flushed["content"].(string) + "\n\n" + archiveSummary
			held := map[string]any{}
			for k, v := range flushed {
				held[k] = v
			}
			held["content"], held["_compressed_summary"] = merged, true
			return []map[string]any{held}, []any{merged}
		},
		"shortened_suffix":        shortenedAfterFlushedHold("fix bug in parser", "fix bug"),
		"shortened_instruction":   shortenedAfterFlushedHold("yes, but first delete the prod database", "yes"),
		"shortened_line_digest":   shortenedAfterFlushedHold("fix bug\nthen drop the prod table", "fix bug"),
		"shortened_line_uid_only": shortenedAfterFlushedHold("fix bug\nthen drop the prod table", "fix bug", "_row_id", DBRowSnapshot),
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			db, sid := archiveDB(t)
			watermark := archiveWatermark(t, db, sid)
			compacted, expected := build(t, db, sid)
			var versions []string
			for _, r := range archiveRows(t, db, sid) {
				if r.active == 1 && r.id > *watermark {
					versions = append(versions, r.content.(string))
				}
			}
			archiveCompact(t, db, sid, compacted, ArchiveCompactOptions{Watermark: watermark, TailCount: 1})
			if got := archiveContents(archiveConversation(t, db, sid, false)); !slices.Equal(got, expected) {
				t.Fatalf("live %q want %q", got, expected)
			}
			var display []string
			for _, c := range archiveDisplayContents(t, db, sid) {
				display = append(display, fmt.Sprint(c))
			}
			joined := strings.Join(display, "\n")
			if len(versions) == 0 {
				t.Fatal("no tail versions")
			}
			for _, v := range versions {
				if !strings.Contains(joined, v) {
					t.Fatalf("version %q not displayed in %q", v, joined)
				}
			}
		})
	}
}

func archiveReplayed(t *testing.T, db *SessionDB, sid string) []any {
	var out []any
	for _, m := range archiveConversation(t, db, sid, false) {
		content := m["content"]
		if side, ok := m["api_content"].(string); ok && side != "" && (m["role"] == "user" || m["role"] == "assistant") {
			content = side
		}
		out = append(out, content)
	}
	return out
}

func TestMatchingTextNeverClaimsAnIndependentAppend(t *testing.T) {
	db, sid := archiveDB(t)
	readAppend(t, db, sid, map[string]any{"role": "assistant", "content": "continue"})
	watermark := archiveWatermark(t, db, sid)
	readAppend(t, db, sid, map[string]any{"role": "assistant", "content": "continue"})
	archiveCompact(t, db, sid, []map[string]any{archiveSummaryMsg(), {"role": "assistant", "content": "continue"}}, ArchiveCompactOptions{Watermark: watermark, TailCount: 1})
	if got := archiveReplayed(t, db, sid); !slices.Equal(got, []any{archiveSummary, "continue", "continue"}) {
		t.Fatalf("replayed %v", got)
	}
}

func TestClaimKeepsTheRowsCurrentReplaySidecar(t *testing.T) {
	db, sid := archiveDB(t)
	watermark := archiveWatermark(t, db, sid)
	flushed := map[string]any{"role": "user", "content": "hello", "api_content": "first exact provider input"}
	archiveBatch(t, db, sid, flushed)
	held := map[string]any{}
	for k, v := range flushed {
		held[k] = v
	}
	rowID, _ := pyIntID(flushed["_row_id"])
	if _, err := db.SetMessageAPIContent(context.Background(), sid, rowID, "hello", "new exact provider input"); err != nil {
		t.Fatal(err)
	}
	archiveCompact(t, db, sid, []map[string]any{archiveSummaryMsg(), held}, ArchiveCompactOptions{Watermark: watermark, TailCount: 1})
	if got := archiveReplayed(t, db, sid); !slices.Contains(got, any("new exact provider input")) {
		t.Fatalf("replayed %v", got)
	}
}

func TestSetMessageAPIContentLeavesOlderIdenticalRow(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	ctx := context.Background()
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "ok", "api_content": "ok\n\nTURN-1"})
	readAppend(t, db, "s1", map[string]any{"role": "assistant", "content": "reply"})
	turn2 := readAppend(t, db, "s1", map[string]any{"role": "user", "content": "ok"})
	if n, err := db.SetMessageAPIContent(ctx, "s1", turn2, "ok", "ok\n\nTURN-2"); err != nil || n != 1 {
		t.Fatalf("rowcount %d %v", n, err)
	}
	rows, _ := db.GetMessages(ctx, "s1", GetMessagesOptions{})
	if rows[0].Vals["api_content"] != "ok\n\nTURN-1" || rows[2].Vals["api_content"] != "ok\n\nTURN-2" {
		t.Fatalf("sidecars %v %v", rows[0].Vals["api_content"], rows[2].Vals["api_content"])
	}
}

func TestSetMessageAPIContentGuards(t *testing.T) {
	db := usageDB(t)
	ctx := context.Background()
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	readSession(t, db, NewSession{ID: "s2", Source: "cli"})
	rowID := readAppend(t, db, "s1", map[string]any{"role": "user", "content": "hello"})
	for _, c := range []struct {
		sid     string
		id      int64
		content string
	}{{"s2", rowID, "hello"}, {"s1", rowID, "other"}, {"s1", rowID + 999, "hello"}} {
		if n, err := db.SetMessageAPIContent(ctx, c.sid, c.id, c.content, "x"); err != nil || n != 0 {
			t.Fatalf("%+v wrote %d %v", c, n, err)
		}
	}
	rows, _ := db.GetMessages(ctx, "s1", GetMessagesOptions{})
	if rows[0].Vals["api_content"] != nil {
		t.Fatal("sidecar written")
	}
	archiveCompact(t, db, "s1", []map[string]any{{"role": "user", "content": "hello"}}, ArchiveCompactOptions{})
	if n, _ := db.SetMessageAPIContent(ctx, "s1", rowID, "hello", "x"); n != 0 {
		t.Fatal("archived row written")
	}
}
