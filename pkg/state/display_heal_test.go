package state

import (
	"context"
	"testing"
)

// Carried compaction whose live reply copy is then split into its own display slot by a legacy writer
func splitCarry(t *testing.T, sid, reply string) *SessionDB {
	t.Helper()
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: sid, Source: "desktop"})
	ts := 1727000000.0
	readAppend(t, db, sid, map[string]any{"role": "user", "content": "q", "timestamp": ts})
	readAppend(t, db, sid, map[string]any{"role": "assistant", "content": reply, "timestamp": ts + 1})
	live := archiveConversation(t, db, sid, false)
	archiveCompact(t, db, sid, append([]map[string]any{{"role": "user", "content": "[summary]"}}, live[len(live)-2:]...), ArchiveCompactOptions{TailCount: 2})
	row, err := db.readOne(ctx, "SELECT MAX(id) AS m FROM messages WHERE session_id = ? AND active = 1 AND role = 'assistant'", sid)
	if err != nil {
		t.Fatal(err)
	}
	copyID := row.Vals["m"]
	profileExec(t, db, "UPDATE messages SET display_order = id, display_identity = zeroblob(32) WHERE id = ?", copyID)
	profileExec(t, db, "UPDATE messages SET compacted = 1 WHERE session_id = ? AND role = 'assistant' AND active = 0 AND id <> ?", sid, copyID)
	return db
}

func splitCompactAgain(t *testing.T, db *SessionDB, sid, reply string) {
	t.Helper()
	var tail []map[string]any
	for _, m := range archiveConversation(t, db, sid, false) {
		if m["role"] == "assistant" && m["content"] == reply {
			tail = []map[string]any{m}
		}
	}
	archiveCompact(t, db, sid, append([]map[string]any{{"role": "user", "content": "[summary 2]"}}, tail...), ArchiveCompactOptions{})
}

func splitReplyCount(t *testing.T, db *SessionDB, sid, reply string, o GetMessagesOptions) int {
	t.Helper()
	o.IncludeCompacted = true
	rows, err := db.GetMessages(context.Background(), sid, o)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.Vals["role"] == "assistant" && r.Vals["content"] == reply {
			n++
		}
	}
	return n
}

func TestSplitTextReplyProjectsOnce(t *testing.T) {
	db := splitCarry(t, "chat", "same reply")
	if n := splitReplyCount(t, db, "chat", "same reply", GetMessagesOptions{}); n != 2 {
		t.Fatalf("before heal = %d", n)
	}
	splitCompactAgain(t, db, "chat", "same reply")
	ten := 10
	for _, o := range []GetMessagesOptions{{}, {Latest: true, Limit: &ten}, {Limit: &ten}} {
		if n := splitReplyCount(t, db, "chat", "same reply", o); n != 1 {
			t.Fatalf("%+v shows %d replies", o, n)
		}
	}
}

func TestSplitEmptyReplyProjectsOnce(t *testing.T) {
	db := splitCarry(t, "chat", "")
	if n := splitReplyCount(t, db, "chat", "", GetMessagesOptions{}); n != 2 {
		t.Fatalf("before heal = %d", n)
	}
	splitCompactAgain(t, db, "chat", "")
	if n := splitReplyCount(t, db, "chat", "", GetMessagesOptions{}); n != 1 {
		t.Fatalf("after heal = %d", n)
	}
}

func TestSplitHealIsDurableNotPerRead(t *testing.T) {
	db := splitCarry(t, "chat", "same reply")
	splitCompactAgain(t, db, "chat", "same reply")
	if n := splitReplyCount(t, db, "chat", "same reply", GetMessagesOptions{}); n != 1 {
		t.Fatalf("after heal = %d", n)
	}
	orders, err := db.readAll(context.Background(), "SELECT DISTINCT display_order FROM messages WHERE session_id = ? AND role = 'assistant' AND content = ? "+
		"AND (active = 1 OR compacted = 1)", "chat", encodeMessageContent("same reply"))
	if err != nil || len(orders) != 1 {
		t.Fatalf("orders = %d err %v", len(orders), err)
	}
}

type unaddressedRow struct {
	id                   int64
	content, tokens, uid any
}

func unaddressedRows(t *testing.T, db *SessionDB, sid string) []unaddressedRow {
	t.Helper()
	rows, err := db.readAll(context.Background(), "SELECT id, content, token_count, message_uid FROM messages WHERE session_id = ? AND active = 1 ORDER BY id", sid)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]unaddressedRow, len(rows))
	for i, r := range rows {
		out[i] = unaddressedRow{r.Vals["id"].(int64), r.Vals["content"], r.Vals["token_count"], r.Vals["message_uid"]}
	}
	return out
}

func unaddressedBatch(t *testing.T, db *SessionDB, sid string, msgs []map[string]any) int {
	t.Helper()
	n, err := db.AppendMessagesBatch(context.Background(), sid, msgs, AppendGuards{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMutatedReplayRewritesItsOriginalRow(t *testing.T) {
	cases := []struct {
		name    string
		rowIDs  bool
		content string
	}{{"unaddressed", false, "after repair"}, {"row-addressed", true, "before repair"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sid := "mutated-replay"
			db := usageDB(t)
			readSession(t, db, NewSession{ID: sid, Source: "desktop"})
			if n := unaddressedBatch(t, db, sid, []map[string]any{{"role": "assistant", "content": "before repair", "timestamp": 1000.0, "token_count": int64(42)}}); n != 1 {
				t.Fatalf("appended %d", n)
			}
			before := unaddressedRows(t, db, sid)
			restored, err := db.GetMessagesAsConversation(context.Background(), sid, ConversationOptions{RepairAlternation: true, IncludeRowIDs: c.rowIDs})
			if err != nil {
				t.Fatal(err)
			}
			_, hasSnapshot := restored[0][DBRowSnapshot]
			if restored[0][MessageUID] != before[0].uid || hasSnapshot == c.rowIDs {
				t.Fatalf("restored = %v", restored[0])
			}
			restored[0]["content"] = "after repair"
			restored[0]["token_count"] = nil
			delete(restored[0], DBPersistedMarker)
			if n := unaddressedBatch(t, db, sid, restored); n != 0 {
				t.Fatalf("appended %d", n)
			}
			after := unaddressedRows(t, db, sid)
			if len(after) != 1 || after[0].id != before[0].id || after[0].uid != before[0].uid || after[0].content != c.content || after[0].tokens != int64(42) {
				t.Fatalf("after = %+v", after)
			}
		})
	}
}

func TestFreshIdenticalMessageIsNeverAdoptedByPayload(t *testing.T) {
	sid := "identical-twins"
	db := usageDB(t)
	readSession(t, db, NewSession{ID: sid, Source: "desktop"})
	if n := unaddressedBatch(t, db, sid, []map[string]any{{"role": "user", "content": "same", "timestamp": 3000.0}}); n != 1 {
		t.Fatalf("appended %d", n)
	}
	uid := unaddressedRows(t, db, sid)[0].uid
	if n := unaddressedBatch(t, db, sid, []map[string]any{{"role": "user", "content": "same", "timestamp": 3000.0, MessageUID: uid}}); n != 1 {
		t.Fatalf("appended %d", n)
	}
	if n := len(unaddressedRows(t, db, sid)); n != 2 {
		t.Fatalf("rows = %d", n)
	}
}
