package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Column names of a table in a fresh current schema store
func lfSchemaColumns(t *testing.T, conn state.SQLiteConn, table string) []string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	columns, _ := rows.Columns()
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatal(err)
		}
		switch name := values[1].(type) {
		case string:
			names = append(names, name)
		case []byte:
			names = append(names, string(name))
		}
	}
	return names
}

// Fresh current schema store at path
func lfFreshStore(t *testing.T, path string) {
	t.Helper()
	caseHome(t)
	db, err := state.NewSessionDB(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// Maps synthetic lost_and_found cells into a fresh store, blanking timestamps the way a shifted physical layout does
func lfMapSalvageRows(t *testing.T, tmp string, blankSessionStartedAt, blankMessageTimestamp bool) state.SQLiteConn {
	t.Helper()
	ctx := context.Background()
	schemaRef := filepath.Join(tmp, "schema-ref.db")
	lfFreshStore(t, schemaRef)
	schema := recoveryTestConn(t, schemaRef)
	sessionsColumns := lfSchemaColumns(t, schema, "sessions")
	messagesColumns := lfSchemaColumns(t, schema, "messages")
	schema.Close()
	width := len(sessionsColumns)
	lfPath := filepath.Join(tmp, "lost_and_found.db")
	lfConn := recoveryTestConn(t, lfPath)
	cells := make([]string, width)
	for i := range cells {
		cells[i] = fmt.Sprintf("c%d", i)
	}
	recoveryTestExec(t, lfConn, "CREATE TABLE lost_and_found (rootpgno INTEGER, pgno INTEGER, nfield INTEGER, id INTEGER, "+strings.Join(cells, ", ")+")")
	marks := strings.TrimSuffix(strings.Repeat("?, ", 4+width), ", ")
	insert := func(nfield, rowid int, values []any) {
		args := []any{2, 5, nfield, rowid}
		args = append(args, values...)
		for len(args) < 4+width {
			args = append(args, nil)
		}
		recoveryTestExec(t, lfConn, "INSERT INTO lost_and_found VALUES ("+marks+")", args...)
	}
	for index := 0; index < 3; index++ {
		sid := fmt.Sprintf("20260101_01010%d_aaa00%d", index, index)
		row := map[string]any{"id": sid, "source": "telegram", "session_key": "agent:main:telegram:dm:" + sid, "chat_type": "dm",
			"model": "gpt-4.1", "model_config": "{}", "started_at": 1_754_000_000.0, "ended_at": 1_754_000_600.0, "end_reason": "completed",
			"message_count": 2, "cwd": "/home/user/project", "title": "mis-mapped probe " + sid, "title_source": "llm", "api_call_count": 1}
		if blankSessionStartedAt {
			row["started_at"] = nil
		}
		values := make([]any, len(sessionsColumns))
		for i, column := range sessionsColumns {
			values[i] = row[column]
		}
		insert(width, index+1, values)
	}
	for index := 0; index < 2; index++ {
		message := map[string]any{"id": nil, "session_id": "20260101_010100_aaa000", "role": "user", "content": "payload",
			"timestamp": 1_754_000_100.0 + float64(index)}
		if blankMessageTimestamp {
			message["timestamp"] = nil
		}
		values := make([]any, 23)
		for i, column := range messagesColumns[:23] {
			values[i] = message[column]
		}
		insert(23, 100+index, values)
	}
	lfConn.Close()
	output := filepath.Join(tmp, "mapped.db")
	lfFreshStore(t, output)
	lfConn = recoveryTestConn(t, lfPath)
	dest := recoveryTestConn(t, output)
	recoveryTestExec(t, dest, "PRAGMA foreign_keys=OFF")
	if _, err := mapLostAndFoundRows(ctx, lfConn, dest); err != nil {
		t.Fatal(err)
	}
	lfConn.Close()
	dest.Close()
	return recoveryTestConn(t, output)
}

func lfCount(t *testing.T, conn state.SQLiteConn, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func lfPlausibility(t *testing.T, conn state.SQLiteConn) []any {
	t.Helper()
	errs, err := lostAndFoundPlausibilityErrors(conn)
	if err != nil {
		t.Fatal(err)
	}
	return errs
}

func TestPlausibilityGateFlagsPositionalMisMapping(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	conn := lfMapSalvageRows(t, t.TempDir(), true, false)
	if lfCount(t, conn, "SELECT COUNT(*) FROM sessions") != 3 || lfCount(t, conn, "SELECT COUNT(*) FROM sessions WHERE started_at = 0.0") != 3 {
		t.Fatal("mapper did not insert the shifted rows")
	}
	if errs := lfPlausibility(t, conn); len(errs) != 1 || !strings.Contains(fmt.Sprint(errs[0]), "sessions.started_at") {
		t.Fatalf("errors %v", errs)
	}
}

func TestPlausibilityGateFlagsMisMappedMessageTimestamps(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	conn := lfMapSalvageRows(t, t.TempDir(), false, true)
	if errs := lfPlausibility(t, conn); len(errs) != 1 || !strings.Contains(fmt.Sprint(errs[0]), "messages.timestamp") {
		t.Fatalf("errors %v", errs)
	}
}

func TestPlausibilityGatePassesCorrectlyMappedSalvage(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	conn := lfMapSalvageRows(t, t.TempDir(), false, false)
	recoveryTestExec(t, conn, "UPDATE sessions SET started_at = 0.0 WHERE id = ?", "20260101_010101_aaa001")
	if errs := lfPlausibility(t, conn); len(errs) != 0 {
		t.Fatalf("errors %v", errs)
	}
}

func TestPlausibilityGateSessionScope(t *testing.T) {
	t.Run("test_plausibility_gate_ignores_stub_only_sessions", func(t *testing.T) {
		recoveryWithoutCJKExtension(t)
		output := filepath.Join(t.TempDir(), "recovered-metadata.db")
		lfFreshStore(t, output)
		conn := recoveryTestConn(t, output)
		now := 1_750_000_000.0
		recoveryTestExec(t, conn, "INSERT INTO sessions (id, source, started_at, title) VALUES (?, ?, ?, ?)",
			"20260812_140000_aaa000", "recovered", 0.0, "[best-effort recovered 1] session metadata was unreadable")
		recoveryTestExec(t, conn, "INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, ?, ?, ?)",
			"20260812_140000_aaa000", "user", "hi", now)
		if errs := lfPlausibility(t, conn); len(errs) != 0 {
			t.Fatalf("metadata only errors %v", errs)
		}
		recoveryTestExec(t, conn, "INSERT INTO sessions (id, source, started_at, title) VALUES (?, ?, ?, ?)", "20260812_140001_aaa001", "cli", now, nil)
		if errs := lfPlausibility(t, conn); len(errs) != 0 {
			t.Fatalf("mapped row errors %v", errs)
		}
		recoveryTestExec(t, conn, "UPDATE sessions SET started_at = 0.0 WHERE id = '20260812_140001_aaa001'")
		if errs := lfPlausibility(t, conn); len(errs) != 1 || !strings.Contains(fmt.Sprint(errs[0]), "sessions.started_at") {
			t.Fatalf("errors %v", errs)
		}
	})
}
