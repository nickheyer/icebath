package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Three dated sessions of nine messages vacuumed into one rollback journal file whose schema page is then damaged
func lfMakeSchemaUnreadableSource(t *testing.T, path string) (int64, int64) {
	t.Helper()
	caseHome(t)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		sid := fmt.Sprintf("20260812_1353%02d_abc%03x", n, n)
		if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Cwd: fmt.Sprintf("/tmp/laf-%d", n)}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SetSessionTitle(ctx, sid, fmt.Sprintf("LAF %d", n)); err != nil {
			t.Fatal(err)
		}
		for m := 0; m < 9; m++ {
			role := "assistant"
			if m%2 == 0 {
				role = "user"
			}
			if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": role, "content": fmt.Sprintf("lost-and-found payload %d %d", n, m)}, state.AppendGuards{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	db.Close()
	func() {
		conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		for _, stmt := range []string{"PRAGMA wal_checkpoint(TRUNCATE)", "PRAGMA journal_mode=DELETE", "VACUUM"} {
			recoveryTestExec(t, conn, stmt)
		}
	}()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:16]) != "SQLite format 3\x00" || !strings.ContainsRune("\x02\x05\x0a\x0d", rune(data[100])) {
		t.Fatalf("unexpected page one header %x", data[100])
	}
	data[103], data[104] = 0xff, 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return 3, 27
}

func TestLostAndFoundLaneRecoversSchemaUnreadableSource(t *testing.T) {
	caseHome(t)
	recoveryWithoutCJKExtension(t)
	if _, ok := findSQLite3CLI(); !ok {
		t.Skip("sqlite3 CLI not on PATH; .recover is a shell-only feature")
	}
	dir := t.TempDir()
	source, output := filepath.Join(dir, "schemaless.db"), filepath.Join(dir, "schemaless-recovered.db")
	wantSessions, wantMessages := lfMakeSchemaUnreadableSource(t, source)
	probe := recoveryTestConn(t, source)
	if _, _, err := recoveryQueryOne(probe, "SELECT COUNT(*) FROM messages"); !isSQLiteDatabaseError(err) {
		t.Fatalf("schema still readable: %v", err)
	}
	probe.Close()
	report, err := recoverSessionDatabase(context.Background(), source, output, dir, 1000, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryAt(t, report, "mode") != "lost_and_found_salvage" || recoveryAt(t, report, "best_effort") != true || recoveryAt(t, report, "partial") != true ||
		recoveryAt(t, report, "complete") != false || recoveryAt(t, report, "installed") != false {
		t.Fatalf("verdict %v", report)
	}
	if !reflect.DeepEqual(recoveryAt(t, report, "unreadable_schemas"), []any{"sessions", "messages"}) {
		t.Fatalf("unreadable %v", recoveryAt(t, report, "unreadable_schemas"))
	}
	bestEffort := false
	for _, w := range recoveryAt(t, report, "verification", "warnings").([]any) {
		bestEffort = bestEffort || strings.Contains(w.(string), "BEST-EFFORT")
	}
	if !bestEffort {
		t.Fatalf("warnings %v", recoveryAt(t, report, "verification", "warnings"))
	}
	conn := recoveryTestConn(t, output)
	state.LoadFTS5CJKExtension(context.Background(), conn)
	integrity, err := recoveryQueryAll(conn, "PRAGMA integrity_check")
	if err != nil || !reflect.DeepEqual(integrity, [][]any{{"ok"}}) {
		t.Fatalf("integrity %v %v", integrity, err)
	}
	if fks, err := recoveryQueryAll(conn, "PRAGMA foreign_key_check"); err != nil || len(fks) != 0 {
		t.Fatalf("foreign keys %v %v", fks, err)
	}
	sessions, _ := recoveryCountRows(conn, "sessions")
	messages, _ := recoveryCountRows(conn, "messages")
	orphans, _, _ := recoveryQueryOne(conn, "SELECT COUNT(*) FROM messages WHERE session_id NOT IN (SELECT id FROM sessions)")
	matches, _, err := recoveryQueryOne(conn, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?", "payload")
	if err != nil {
		t.Fatal(err)
	}
	if sessions != wantSessions || messages != wantMessages || orphans[0] != int64(0) || matches[0] != wantMessages {
		t.Fatalf("sessions %d messages %d orphans %v matches %v", sessions, messages, orphans[0], matches[0])
	}
	conn.Close()
	recovered, err := state.NewSessionDB(context.Background(), output, false)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	limit := 10
	rows, err := recovered.ListSessionsRich(context.Background(), state.ListSessionsRichOptions{Limit: &limit})
	if err != nil || int64(len(rows)) != wantSessions {
		t.Fatalf("listed %d %v", len(rows), err)
	}
}

func TestProbePopulatedEdgeCapsSyntheticDomain(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	conn := recoveryTestConn(t, filepath.Join(t.TempDir(), "clean.db"))
	recoveryTestExec(t, conn, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	for i := 1; i <= 100; i++ {
		recoveryTestExec(t, conn, "INSERT INTO t (id, v) VALUES (?, ?)", i, fmt.Sprintf("value %d", i))
	}
	probe, err := recoveryProbePopulatedEdge(conn, "t", "high", 1)
	if err != nil {
		t.Fatal(err)
	}
	bound, _ := probe.GetOr("bound", nil).(int64)
	if probe.GetOr("capped", nil) != true || bound < 100 || bound >= 10_000 || recoveryCounter(probe, "probes") > 64 {
		t.Fatalf("high probe %v", probe)
	}
	low, err := recoveryProbePopulatedEdge(conn, "t", "low", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lowBound, _ := low.GetOr("bound", nil).(int64); low.GetOr("capped", nil) != true || lowBound > 1 {
		t.Fatalf("low probe %v", low)
	}
}

func TestUnreadableSchemaWithoutCliNamesTheSqlite3Requirement(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	dir := t.TempDir()
	source, output := filepath.Join(dir, "schemaless.db"), filepath.Join(dir, "schemaless-recovered.db")
	lfMakeSchemaUnreadableSource(t, source)
	t.Setenv("PATH", t.TempDir())
	_, err := recoverSessionDatabase(context.Background(), source, output, dir, 1000, nil, true)
	var recoveryErr *sessionRecoveryError
	if !errors.As(err, &recoveryErr) || recoveryErr.kind != "source" {
		t.Fatalf("error %v", err)
	}
	message := err.Error()
	for _, want := range []string{"sessions", "messages", "sqlite3", ".recover"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message lacks %q: %s", want, message)
		}
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("output created: %v", err)
	}
}

func TestClassifyLostAndFoundRowSentinels(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	nones := func(n int) []any { return make([]any, n) }
	cases := []struct {
		nfield int64
		cells  []any
		want   string
	}{
		{23, []any{nil, "20260101_010101_aaa001", "user", "hi"}, "messages"},
		{55, append([]any{"20260101_010101_aaa001", "cli"}, nones(53)...), "sessions"},
		{52, append([]any{"20260101_010101_aaa001", "discord"}, nones(50)...), "sessions"},
		{14, append([]any{"20250101_010101_zzz999", "cli"}, nones(12)...), "sessions"},
		{18, append([]any{"20260101_010101_aaa001", "gpt-x"}, nones(16)...), "session_model_usage"},
		{3, []any{"random", "noise", int64(42)}, ""},
		{55, append([]any{"not-a-session-id", "cli"}, nones(53)...), ""},
		{23, []any{nil, "sess", "not-a-role", "x"}, ""},
		{0, []any{}, ""},
	}
	for _, c := range cases {
		if got := classifyLostAndFoundRow(c.nfield, c.cells); got != c.want {
			t.Fatalf("classify(%d, %v) = %q, want %q", c.nfield, c.cells[:min(len(c.cells), 3)], got, c.want)
		}
	}
}
