package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func sidecarDB(t *testing.T) *SessionDB {
	t.Helper()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	return db
}

func sidecarSet(t *testing.T, db *SessionDB, content, api string) int64 {
	t.Helper()
	n, err := db.SetLatestUserAPIContent(context.Background(), "s1", content, api)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSidecarRoundTripIsVerbatimNotSanitized(t *testing.T) {
	db := sidecarDB(t)
	sent := "  hello\n\n<memory-context>\nrecalled\n</memory-context>\n"
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "hello", "api_content": sent})
	msgs := archiveConversation(t, db, "s1", false)
	if msgs[0]["content"] != "hello" || msgs[0]["api_content"] != sent {
		t.Fatalf("msgs = %#v", msgs)
	}
}

func TestReconciliationAddsAPIContentColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, raw, `
            CREATE TABLE schema_version (version INTEGER NOT NULL);
            CREATE TABLE sessions (
                id TEXT PRIMARY KEY,
                source TEXT NOT NULL,
                parent_session_id TEXT,
                started_at REAL NOT NULL
            );
            CREATE TABLE messages (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                session_id TEXT NOT NULL,
                role TEXT NOT NULL,
                content TEXT,
                tool_call_id TEXT,
                tool_calls TEXT,
                tool_name TEXT,
                timestamp REAL NOT NULL,
                token_count INTEGER,
                finish_reason TEXT,
                reasoning TEXT
            );`)
	mustExec(t, raw, "INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, ?, ?, ?)", "s1", "user", "old row", 1000.0)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db := usageOpen(t, path)
	cols, err := db.readAll(context.Background(), `PRAGMA table_info("messages")`)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cols {
		if c.Vals["name"] == "api_content" {
			found = true
		}
	}
	if !found {
		t.Fatal("api_content column missing after reconciliation")
	}
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "new", "api_content": "new+ctx"})
	msgs := archiveConversation(t, db, "s1", false)
	if _, has := msgs[0]["api_content"]; has || len(msgs) != 2 || msgs[1]["api_content"] != "new+ctx" {
		t.Fatalf("msgs = %#v", msgs)
	}
}

func TestSetLatestUserAPIContentUpdatesNewestActiveUserRow(t *testing.T) {
	db := sidecarDB(t)
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "q1"})
	readAppend(t, db, "s1", map[string]any{"role": "assistant", "content": "a1"})
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "q2"})
	if n := sidecarSet(t, db, "q2", "q2\n\nCTX"); n != 1 {
		t.Fatalf("rowcount = %d", n)
	}
	msgs := archiveConversation(t, db, "s1", false)
	if _, has := msgs[0]["api_content"]; has || msgs[2]["api_content"] != "q2\n\nCTX" {
		t.Fatalf("msgs = %#v", msgs)
	}
}

func TestSetLatestUserAPIContentMismatchWritesNothing(t *testing.T) {
	db := sidecarDB(t)
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "q1"})
	if n := sidecarSet(t, db, "other", "other+CTX"); n != 0 {
		t.Fatalf("rowcount = %d", n)
	}
	if _, has := archiveConversation(t, db, "s1", false)[0]["api_content"]; has {
		t.Fatal("api_content written on a content mismatch")
	}
}

const loneSurrogateDirty = "scraped \xed\xa0\xb5 price"

func TestAppendMessageSurvivesLoneSurrogateContent(t *testing.T) {
	db := sidecarDB(t)
	readAppend(t, db, "s1", map[string]any{"role": "assistant", "content": "hello world"})
	readAppend(t, db, "s1", map[string]any{"role": "tool", "content": loneSurrogateDirty, "tool_name": "web_search"})
	rows, err := db.GetMessages(context.Background(), "s1", GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].Vals["content"] != "scraped � price" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestSetLatestUserAPIContentSurvivesLoneSurrogate(t *testing.T) {
	db := sidecarDB(t)
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "turn text"})
	if n := sidecarSet(t, db, "turn text", loneSurrogateDirty); n != 1 {
		t.Fatalf("rowcount = %d", n)
	}
}
