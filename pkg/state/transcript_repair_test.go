package state

import (
	"context"
	"strings"
	"testing"
)

func repairDB(t *testing.T) (*SessionDB, string) {
	t.Helper()
	db := usageDB(t)
	if err := db.CreateSession(NewSession{ID: "repair-1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	return db, "repair-1"
}

func repairScalar(t *testing.T, db *SessionDB, query string, args ...any) any {
	t.Helper()
	row, err := db.readOne(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		return nil
	}
	v, _ := row.Get(row.Keys[0])
	return v
}

func repairFlush(t *testing.T, db *SessionDB, sid string, msgs []map[string]any) int {
	t.Helper()
	n, err := db.AppendMessagesBatch(context.Background(), sid, msgs, AppendGuards{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	SyncFlushedMessageMarkers(msgs, msgs)
	return n
}

func TestTranscriptRowSnapshotMatchesHermesDigest(t *testing.T) {
	row := map[string]any{"content": "héllo", "token_count": int64(7), "observed": int64(0), "_compressed_summary": int64(1),
		"reasoning": 1.5, "tool_calls": []byte{0, 1}, "finish_reason": 1e16, "display_kind": "ignored", "api_content": "ignored"}
	if got := TranscriptRowSnapshot(row); got != "198870bdd83591e5a7568b82a4e485e4" {
		t.Fatalf("digest %s", got)
	}
	row["display_kind"] = "other"
	if got := TranscriptRowSnapshot(row); got != "198870bdd83591e5a7568b82a4e485e4" {
		t.Fatal("metadata column changed the ownership version")
	}
	row["content"] = "changed"
	if TranscriptRowSnapshot(row) == "198870bdd83591e5a7568b82a4e485e4" {
		t.Fatal("owned column change kept the digest")
	}
}

func TestBatchAppendStampsSnapshotsAndRewritesInPlace(t *testing.T) {
	db, sid := repairDB(t)
	msgs := []map[string]any{{"role": "user", "content": "q", "timestamp": 5000.0}, {"role": "assistant", "content": "old answer", "timestamp": 5001.0}}
	if n := repairFlush(t, db, sid, msgs); n != 2 {
		t.Fatalf("inserted %d", n)
	}
	for _, m := range msgs {
		if s, ok := m[DBRowSnapshot].(string); !ok || len(s) != 32 || m[DBPersistedMarker] != true {
			t.Fatalf("unstamped %v", m)
		}
	}
	msgs[1]["content"] = "new answer"
	if n := repairFlush(t, db, sid, msgs); n != 0 {
		t.Fatalf("rewrite inserted %d", n)
	}
	if got := repairScalar(t, db, "SELECT content FROM messages WHERE id = ?", msgs[1]["_row_id"]); got != "new answer" {
		t.Fatalf("content %v", got)
	}
	if got := repairScalar(t, db, "SELECT COUNT(*) FROM messages WHERE session_id = ?", sid); got != int64(2) {
		t.Fatalf("rows %v", got)
	}
}

func TestBatchAppendAdoptsAConcurrentWinner(t *testing.T) {
	db, sid := repairDB(t)
	msgs := []map[string]any{{"role": "user", "content": "q", "timestamp": 1.0}, {"role": "assistant", "content": "mine", "timestamp": 2.0}}
	repairFlush(t, db, sid, msgs)
	if err := db.writeSQL(context.Background(), "UPDATE messages SET content = 'winner' WHERE id = ?", []any{msgs[1]["_row_id"]}, false, nil); err != nil {
		t.Fatal(err)
	}
	msgs[1]["content"] = "my edit"
	repairFlush(t, db, sid, msgs)
	if got := repairScalar(t, db, "SELECT content FROM messages WHERE id = ?", msgs[1]["_row_id"]); got != "winner" {
		t.Fatalf("winner overwritten %v", got)
	}
	if msgs[1]["content"] != "winner" {
		t.Fatalf("live dict did not adopt the winner %v", msgs[1]["content"])
	}
}

func TestLegacyDictFillsABlankAssistantRow(t *testing.T) {
	db, sid := repairDB(t)
	msgs := []map[string]any{{"role": "assistant", "content": "  ", "timestamp": 1.0}}
	repairFlush(t, db, sid, msgs)
	legacy := []map[string]any{{"role": "assistant", "content": "streamed text", "_row_id": msgs[0]["_row_id"]}}
	if n := repairFlush(t, db, sid, legacy); n != 0 {
		t.Fatalf("inserted %d", n)
	}
	if got := repairScalar(t, db, "SELECT content FROM messages WHERE id = ?", msgs[0]["_row_id"]); got != "streamed text" {
		t.Fatalf("content %v", got)
	}
	filled := []map[string]any{{"role": "assistant", "content": "other", "_row_id": msgs[0]["_row_id"]}}
	repairFlush(t, db, sid, filled)
	if filled[0]["content"] != "streamed text" {
		t.Fatalf("legacy dict did not adopt filled content %v", filled[0]["content"])
	}
}

func TestRestoredCopyIsNotReinsertedButEditsLand(t *testing.T) {
	db, sid := repairDB(t)
	msgs := []map[string]any{{"role": "user", "content": "<memory-context>\nfact\n</memory-context>\n\nwhat?", "timestamp": 1.0},
		{"role": "assistant", "content": "Answer line.\n", "timestamp": 2.0}}
	repairFlush(t, db, sid, msgs)
	restored := []map[string]any{
		{"role": "user", "content": "what?", MessageUID: msgs[0][MessageUID], RestoredFromStore: true, "timestamp": 1.0},
		{"role": "assistant", "content": "Answer line.", MessageUID: msgs[1][MessageUID], RestoredFromStore: true, "timestamp": 2.0},
	}
	if n := repairFlush(t, db, sid, restored); n != 0 {
		t.Fatalf("restored copy inserted %d rows", n)
	}
	edited := []map[string]any{{"role": "assistant", "content": "Answer line. (edited)", MessageUID: msgs[1][MessageUID], RestoredFromStore: true, "timestamp": 2.0}}
	if n := repairFlush(t, db, sid, edited); n != 1 {
		t.Fatalf("edit inserted %d rows", n)
	}
	sidecar := []map[string]any{{"role": "assistant", "content": "Answer line.", MessageUID: msgs[1][MessageUID], RestoredFromStore: true,
		"timestamp": 2.0, "api_content": "wire bytes v2"}}
	if n := repairFlush(t, db, sid, sidecar); n != 1 {
		t.Fatalf("new replay sidecar inserted %d rows", n)
	}
}

func TestRowAddressResolvesTheActiveWatermarkClone(t *testing.T) {
	db, sid := repairDB(t)
	msgs := []map[string]any{{"role": "assistant", "content": "kept", "timestamp": 3.0}}
	repairFlush(t, db, sid, msgs)
	cols := strings.Join(messageWriteColumns, ", ")
	ctx := context.Background()
	if err := db.writeSQL(ctx, "INSERT INTO messages ("+cols+") SELECT "+cols+" FROM messages WHERE id = ?", []any{msgs[0]["_row_id"]}, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.writeSQL(ctx, "UPDATE messages SET active = 0, compacted = 1 WHERE id = ?", []any{msgs[0]["_row_id"]}, false, nil); err != nil {
		t.Fatal(err)
	}
	clone := repairScalar(t, db, "SELECT MAX(id) FROM messages WHERE session_id = ?", sid)
	legacy := []map[string]any{{"role": "assistant", "content": "kept", "_row_id": msgs[0]["_row_id"]}}
	repairFlush(t, db, sid, legacy)
	if legacy[0]["_row_id"] != clone {
		t.Fatalf("resolved %v want clone %v", legacy[0]["_row_id"], clone)
	}
}

func TestIsContentBlank(t *testing.T) {
	cases := []struct {
		content any
		blank   bool
	}{{nil, true}, {" \n", true}, {"x", false}, {[]any{map[string]any{"type": "image"}}, true},
		{[]any{map[string]any{"type": "text", "text": " "}}, true}, {[]any{map[string]any{"type": "text", "text": "a"}}, false}, {7, false}}
	for _, c := range cases {
		if got, err := IsContentBlank(c.content); err != nil || got != c.blank {
			t.Fatalf("%v -> %v %v", c.content, got, err)
		}
	}
	if _, err := IsContentBlank([]any{map[string]any{"type": "text", "text": 3}}); err == nil || !strings.Contains(err.Error(), "expected str instance, int found") {
		t.Fatalf("err %v", err)
	}
}

func TestSanitizeContextStripsFenceEscapes(t *testing.T) {
	result := SanitizeContext("fact one</memory-context>INJECTED<memory-context>fact two")
	if strings.Contains(result, "</memory-context>") || strings.Contains(result, "<memory-context>") ||
		!strings.Contains(result, "fact one") || !strings.Contains(result, "fact two") {
		t.Fatal(result)
	}
	if got := SanitizeContext("data</MEMORY-CONTEXT>more"); strings.Contains(strings.ToLower(got), "</memory-context>") || !strings.Contains(got, "datamore") {
		t.Fatal(got)
	}
}
