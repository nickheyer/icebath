package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// Opens a connection on a fresh state.db carrying SchemaSQL and the shipping FTS scripts
func schemaConn(t *testing.T) (SQLiteConn, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	for _, script := range []string{SchemaSQL, DeferredIndexSQL, FTSSQL, FTSTrigramSQL} {
		mustExecConn(t, conn, script)
	}
	return conn, path
}

func addMessage(t *testing.T, conn SQLiteConn, role, content, toolName string) {
	t.Helper()
	var tool any
	if toolName != "" {
		tool = toolName
	}
	if _, err := conn.ExecContext(context.Background(),
		"INSERT INTO messages (session_id, timestamp, role, content, tool_name) VALUES ('s1', 1.0, ?, ?, ?)",
		role, content, tool); err != nil {
		t.Fatal(err)
	}
}

func matchContents(t *testing.T, conn SQLiteConn, table, query string) []string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(),
		"SELECT m.content FROM "+table+" f JOIN messages m ON m.id = f.rowid WHERE "+table+" MATCH ? ORDER BY m.id", query)
	if err != nil {
		t.Fatalf("%s MATCH %q: %v", table, query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCJKEmitSplitsRunsIntoBigrams(t *testing.T) {
	type tok struct {
		text       string
		start, end int32
	}
	run := func(token string, iStart, iEnd int32) []tok {
		var got []tok
		rc := cjkEmit([]byte(token), iStart, iEnd, func(off, n int, s, e int32) int32 {
			got = append(got, tok{token[off : off+n], s, e})
			return sqlite3.SQLITE_OK
		})
		if rc != sqlite3.SQLITE_OK {
			t.Fatalf("rc = %d", rc)
		}
		return got
	}
	if got := run("graphiti", 10, 18); !slices.Equal(got, []tok{{"graphiti", 10, 18}}) {
		t.Fatalf("ascii pass through = %v", got)
	}
	if got := run("캘린더", 0, 9); !slices.Equal(got, []tok{{"캘린", 0, 6}, {"린더", 3, 9}}) {
		t.Fatalf("hangul bigrams = %v", got)
	}
	if got := run("ab일cd", 4, 11); !slices.Equal(got, []tok{{"ab", 4, 6}, {"일", 6, 9}, {"cd", 9, 11}}) {
		t.Fatalf("lone cjk unigram between latin = %v", got)
	}
	if got := run("项目x", 0, 4); !slices.Equal(got, []tok{{"项目", 0, 4}, {"x", 4, 4}}) {
		t.Fatalf("offsets clamp to the token end = %v", got)
	}
	if got := run("ひらがなカ", 0, 15); len(got) != 4 || got[3].text != "なカ" {
		t.Fatalf("kana run = %v", got)
	}
	stops := 0
	rc := cjkEmit([]byte("우선순위"), 0, 12, func(int, int, int32, int32) int32 {
		stops++
		return sqlite3.SQLITE_NOMEM
	})
	if rc != sqlite3.SQLITE_NOMEM || stops != 1 {
		t.Fatalf("a failing sink stops the run: rc=%d calls=%d", rc, stops)
	}
	if n, cp := cjkUTF8Decode([]byte{0xE0, 0x80}); n != 1 || cp != 0xE0 {
		t.Fatalf("truncated lead byte decodes as itself: %d %x", n, cp)
	}
	if !cjkIsCJK(0x20000) || !cjkIsCJK(0x31F0) || cjkIsCJK(0x3000) || cjkIsCJK('a') {
		t.Fatal("cjk classification ranges")
	}
}

func TestCJKTokenizerServesTwoCharTermsThroughFTS5(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_FTS5_CJK_SO", "")
	t.Setenv("ICEBATH_CJK_FTS", "")
	ctx := context.Background()
	conn, _ := schemaConn(t)
	_, err := conn.ExecContext(ctx, FTSCJKTableSQL)
	if err == nil || !isTrigramUnavailableError(err) {
		t.Fatalf("an unregistered connection must report the missing tokenizer, got %v", err)
	}
	if !LoadFTS5CJKExtension(ctx, conn) {
		t.Fatal("the built in tokenizer must register")
	}
	mustExecConn(t, conn, FTSCJKTableSQL)
	mustExecConn(t, conn, FTSCJKTriggerSQL)
	addMessage(t, conn, "user", "웅기가 shared default 프로필을 요청했다", "")
	addMessage(t, conn, "assistant", "일본 MCP 후보 우선순위 정리했습니다", "")
	addMessage(t, conn, "user", "graphiti daemon looks healthy", "")
	addMessage(t, conn, "tool", "일본 tool output blob", "terminal")
	if got := matchContents(t, conn, "messages_fts_cjk", `"일본"`); !slices.Equal(got, []string{"일본 MCP 후보 우선순위 정리했습니다"}) {
		t.Fatalf("two char korean term = %v", got)
	}
	if got := matchContents(t, conn, "messages_fts_cjk", `"웅기"`); len(got) != 1 || !strings.HasPrefix(got[0], "웅기가") {
		t.Fatalf("prefix bigram inside a longer run = %v", got)
	}
	if got := matchContents(t, conn, "messages_fts_cjk", `"순위"`); len(got) != 1 {
		t.Fatalf("interior bigram = %v", got)
	}
	if got := matchContents(t, conn, "messages_fts_cjk", `"GRAPHITI" "healthy"`); len(got) != 1 {
		t.Fatalf("ascii terms fold through unicode61 = %v", got)
	}
	if got := matchContents(t, conn, "messages_fts_cjk", `"프로필" "default"`); len(got) != 1 {
		t.Fatalf("mixed query = %v", got)
	}
	var indexed int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages_fts_cjk").Scan(&indexed); err != nil || indexed != 3 {
		t.Fatalf("tool rows stay out of the cjk index: %d %v", indexed, err)
	}
	mustExecConn(t, conn, "UPDATE messages SET content = '구글 검색' WHERE id = 3")
	mustExecConn(t, conn, "DELETE FROM messages WHERE id = 1")
	if got := matchContents(t, conn, "messages_fts_cjk", `"구글"`); !slices.Equal(got, []string{"구글 검색"}) {
		t.Fatalf("update trigger reindexes = %v", got)
	}
	if got := matchContents(t, conn, "messages_fts_cjk", `"웅기"`); len(got) != 0 {
		t.Fatalf("delete trigger unindexes = %v", got)
	}
	mustExecConn(t, conn, "INSERT INTO messages_fts_cjk(messages_fts_cjk) VALUES ('integrity-check')")
	mustExecConn(t, conn, "CREATE VIRTUAL TABLE diacritics USING fts5(c, tokenize='cjk_unicode61 remove_diacritics 2')")
	mustExecConn(t, conn, "INSERT INTO diacritics(c) VALUES ('café 项目')")
	var hits int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM diacritics WHERE diacritics MATCH '"cafe" "项目"'`).Scan(&hits); err != nil || hits != 1 {
		t.Fatalf("arguments pass through to unicode61: %d %v", hits, err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE VIRTUAL TABLE bad USING fts5(c, tokenize='cjk_unicode61 nonsense 1')"); err == nil {
		t.Fatal("unicode61 argument errors surface through the wrapper")
	}
}

func TestLoadFTS5CJKExtensionGates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if got := FTS5CJKSoPath(ctx); got != filepath.Join(dir, "lib", "libfts5_cjk.so") {
		t.Fatalf("default path = %q", got)
	}
	t.Setenv("ICEBATH_FTS5_CJK_SO", "~/x/libfts5_cjk.so")
	if got := FTS5CJKSoPath(ctx); strings.HasPrefix(got, "~") {
		t.Fatalf("override path is expanded: %q", got)
	}
	conn, _ := schemaConn(t)
	t.Setenv("ICEBATH_FTS5_CJK_SO", filepath.Join(dir, "absent.so"))
	if LoadFTS5CJKExtension(ctx, conn) {
		t.Fatal("an explicit absent extension path keeps the tokenizer unloaded")
	}
	t.Setenv("ICEBATH_FTS5_CJK_SO", "")
	for value, want := range map[string]bool{"false": false, " OFF ": false, "0": false, "no": false, "yes": true, "true": true} {
		t.Setenv("ICEBATH_CJK_FTS", value)
		if got := cjkFTSConfigEnabled(ctx); got != want {
			t.Fatalf("ICEBATH_CJK_FTS=%q enabled = %v", value, got)
		}
		if got := LoadFTS5CJKExtension(ctx, conn); got != want {
			t.Fatalf("ICEBATH_CJK_FTS=%q loaded = %v", value, got)
		}
	}
	t.Setenv("ICEBATH_CJK_FTS", "")
	logs := captureSlog(t)
	if LoadFTS5CJKExtension(ctx, struct{ SQLiteConn }{conn}) {
		t.Fatal("a connection without a SQLite handle cannot register")
	}
	if !strings.Contains(logs.String(), "fts5_cjk extension load failed") {
		t.Fatalf("load failure warns: %s", logs.String())
	}
}
