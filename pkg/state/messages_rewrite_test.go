package state

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

func rewriteTurn(i int, text string) map[string]any {
	role := "user"
	if i%2 == 1 {
		role = "assistant"
	}
	return map[string]any{"role": role, "content": fmt.Sprintf("%s %d", text, i)}
}

func rewriteIDs(t *testing.T, db *SessionDB, sid string, where string) []int64 {
	t.Helper()
	rows, err := db.readAll(context.Background(), "SELECT id FROM messages WHERE session_id = ?"+where+" ORDER BY id", sid)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int64, len(rows))
	for i, r := range rows {
		v, _ := r.Get("id")
		out[i], _ = pyIntID(v)
	}
	return out
}

func rewriteAppend(t *testing.T, db *SessionDB, sid string, msgs []map[string]any) {
	t.Helper()
	for _, m := range msgs {
		if _, err := db.AppendMessage(context.Background(), sid, map[string]any{"role": m["role"], "content": m["content"]}, AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
}

func copyTurns(msgs []map[string]any) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		out[i] = map[string]any{"role": m["role"], "content": m["content"]}
	}
	return out
}

func TestArchiveModeRewindArchivesOnlyTheDroppedSuffix(t *testing.T) {
	db := usageDB(t)
	sid := "rewind-suffix-only"
	if err := db.CreateSession(NewSession{ID: sid, Source: "test"}); err != nil {
		t.Fatal(err)
	}
	var history []map[string]any
	for i := range 50 {
		history = append(history, rewriteTurn(i, "msg"))
	}
	rewriteAppend(t, db, sid, history)
	ctx := context.Background()
	prefix := rewriteIDs(t, db, sid, " AND active = 1")[:40]
	for k := range 3 {
		if err := db.ReplaceMessages(ctx, sid, copyTurns(history[:40]), ReplaceOptions{ActiveOnly: true, ArchiveDropped: true}); err != nil {
			t.Fatal(err)
		}
		if got := rewriteIDs(t, db, sid, " AND active = 1"); !slices.Equal(got, prefix) {
			t.Fatalf("round %d: kept prefix was re-inserted under new ids", k)
		}
		history = history[:40]
		for i := range 10 {
			history = append(history, rewriteTurn(i, fmt.Sprintf("new %d", k)))
		}
		rewriteAppend(t, db, sid, history[40:])
	}
	if n := len(rewriteIDs(t, db, sid, " AND active = 0")); n != 30 {
		t.Fatalf("archived %d", n)
	}
	if n := repairScalar(t, db, "SELECT message_count FROM sessions WHERE id = ?", sid); n != int64(50) {
		t.Fatalf("message_count %v", n)
	}
	rewritten := append(append(copyTurns(history[:30]), rewriteTurn(30, "edited")), copyTurns(history[31:40])...)
	if err := db.ReplaceMessages(ctx, sid, rewritten, ReplaceOptions{ActiveOnly: true, ArchiveDropped: true}); err != nil {
		t.Fatal(err)
	}
	ids := rewriteIDs(t, db, sid, " AND active = 1")
	if !slices.Equal(ids[:30], prefix[:30]) || len(ids) != 40 {
		t.Fatalf("ids %v", ids)
	}
	for _, id := range ids[30:] {
		if slices.Contains(prefix, id) {
			t.Fatalf("rewritten row reused id %d", id)
		}
	}
	if n := len(rewriteIDs(t, db, sid, " AND active = 0")); n != 50 {
		t.Fatalf("archived %d", n)
	}
	if n := repairScalar(t, db, "SELECT message_count FROM sessions WHERE id = ?", sid); n != int64(40) {
		t.Fatalf("message_count %v", n)
	}
}

func TestReloadedSessionPrefixStillMatchesItsRows(t *testing.T) {
	db := usageDB(t)
	sid := "rewind-after-reload"
	if err := db.CreateSession(NewSession{ID: sid, Source: "test"}); err != nil {
		t.Fatal(err)
	}
	var stored, loaded []map[string]any
	for i := range 6 {
		m := rewriteTurn(i, "turn")
		loaded = append(loaded, map[string]any{"role": m["role"], "content": m["content"]})
		m["content"] = m["content"].(string) + "   \n"
		stored = append(stored, m)
	}
	rewriteAppend(t, db, sid, stored)
	prefix := rewriteIDs(t, db, sid, " AND active = 1")[:4]
	if err := db.ReplaceMessages(context.Background(), sid, loaded[:4], ReplaceOptions{ActiveOnly: true, ArchiveDropped: true}); err != nil {
		t.Fatal(err)
	}
	if got := rewriteIDs(t, db, sid, " AND active = 1"); !slices.Equal(got, prefix) {
		t.Fatalf("ids %v want %v", got, prefix)
	}
	if n := len(rewriteIDs(t, db, sid, " AND active = 0")); n != 2 {
		t.Fatalf("archived %d", n)
	}
	if loaded[0]["_row_id"] != prefix[0] {
		t.Fatalf("kept row id not stamped %v", loaded[0]["_row_id"])
	}
}

func TestReplaceMessagesRefusesAMovedActiveSetAndAClosedParent(t *testing.T) {
	db, sid := repairDB(t)
	rewriteAppend(t, db, sid, []map[string]any{rewriteTurn(0, "a")})
	ctx := context.Background()
	err := db.executeWrite(ctx, func(conn SQLiteConn) error {
		return db.ReplaceMessagesInTransaction(ctx, conn, sid, nil, ReplaceOptions{}, []int64{999})
	}, nil)
	if err == nil || err.Error() != "active transcript changed before the rewrite could be persisted" {
		t.Fatalf("err %v", err)
	}
	profileExec(t, db, "UPDATE sessions SET ended_at = 1, end_reason = 'compression' WHERE id = ?", sid)
	if err := db.ReplaceMessages(ctx, sid, nil, ReplaceOptions{}); err == nil {
		t.Fatal("closed compression parent accepted a replacement")
	}
}

func TestRowAddressedMutations(t *testing.T) {
	db, sid := repairDB(t)
	ctx := context.Background()
	rewriteAppend(t, db, sid, []map[string]any{rewriteTurn(0, "raw"), rewriteTurn(1, "reply")})
	ids := rewriteIDs(t, db, sid, "")
	if n, err := db.SetUserMessageContent(ctx, sid, ids[1], "x", nil); err != nil || n != 0 {
		t.Fatalf("assistant row rewritten %d %v", n, err)
	}
	if n, err := db.SetUserMessageContent(ctx, sid, ids[0], "expanded", map[string]any{"k": "v"}); err != nil || n != 1 {
		t.Fatalf("user row %d %v", n, err)
	}
	if got := repairScalar(t, db, "SELECT display_metadata FROM messages WHERE id = ?", ids[0]); got != `{"k": "v"}` {
		t.Fatalf("display metadata %v", got)
	}
	cols := "session_id, role, content, timestamp, active"
	profileExec(t, db, "INSERT INTO messages ("+cols+") SELECT "+cols+" FROM messages WHERE id = ?", ids[0])
	clone := rewriteIDs(t, db, sid, "")[2]
	if n, err := db.DeactivateMessage(ctx, sid, ids[0]); err != nil || n != 1 {
		t.Fatalf("deactivate %d %v", n, err)
	}
	if got, ok, err := db.ResolveActiveRowID(ctx, sid, ids[0]); err != nil || !ok || got != clone {
		t.Fatalf("resolved %d %v %v", got, ok, err)
	}
	if got, ok, _ := db.ResolveActiveRowID(ctx, sid, ids[1]); !ok || got != ids[1] {
		t.Fatalf("active row resolved %d", got)
	}
	if _, ok, _ := db.ResolveActiveRowID(ctx, sid, 9999); ok {
		t.Fatal("missing row resolved")
	}
	profileExec(t, db, "UPDATE messages SET display_kind = 'pivot' WHERE id = ?", ids[1])
	if n, err := db.DeactivateMessagesByDisplayKind(ctx, sid, "pivot"); err != nil || n != 1 {
		t.Fatalf("by kind %d %v", n, err)
	}
	if mark, err := db.GetActiveMessageWatermark(ctx, sid); err != nil || mark != clone {
		t.Fatalf("watermark %d %v", mark, err)
	}
	if archived, err := db.HasArchivedMessages(ctx, sid); err != nil || !archived {
		t.Fatal("no archived rows reported")
	}
	if n, err := db.DisplayMessageCount(ctx, sid); err != nil || n != 1 {
		t.Fatalf("display count %d %v", n, err)
	}
}

func TestFindPRURLMessagesScansToolResultsOldestFirst(t *testing.T) {
	db, sid := repairDB(t)
	ctx := context.Background()
	for _, c := range []string{"https://github.com/o/r/pull/1", "no link", "https://github.com/o/r/pull/2"} {
		if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": "tool", "content": c, "tool_call_id": "c"}, AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	rewriteAppend(t, db, sid, []map[string]any{{"role": "user", "content": "/pull/9"}})
	got, err := db.FindPRURLMessages(ctx, []string{sid, ""})
	if err != nil || len(got) != 2 || got[1]["content"] != "https://github.com/o/r/pull/2" || got[0]["session_id"] != sid {
		t.Fatalf("%v %v", got, err)
	}
}
