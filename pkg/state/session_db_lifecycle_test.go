package state

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const siblingReaderHelperEnv = "ICEBATH_SIBLING_READER_HELPER"

// Connection that records every statement it runs, the sqlite3 set_trace_callback view
type recordingConn struct {
	SQLiteConn
	mu   sync.Mutex
	sqls []string
}

func (r *recordingConn) record(query string) {
	r.mu.Lock()
	r.sqls = append(r.sqls, query)
	r.mu.Unlock()
}

func (r *recordingConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	r.record(query)
	return r.SQLiteConn.ExecContext(ctx, query, args...)
}

func (r *recordingConn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	r.record(query)
	return r.SQLiteConn.QueryContext(ctx, query, args...)
}

func (r *recordingConn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	r.record(query)
	return r.SQLiteConn.QueryRowContext(ctx, query, args...)
}

// Wraps the writer connection of db in a recorder
func traceWriter(db *SessionDB) *recordingConn {
	db.mu.Lock()
	defer db.mu.Unlock()
	rec := &recordingConn{SQLiteConn: db.conn}
	db.setConnLocked(rec)
	return rec
}

// Snapshot of the live connection registry
func liveConnectionsSnapshot() map[string]int {
	liveMu.Lock()
	defer liveMu.Unlock()
	return maps.Clone(liveConnections)
}

func TestWritableStateDBTightensExistingLooseMode(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "state.db")
	mustSessionDB(t, path, false).Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	db := mustSessionDB(t, path, false)
	defer db.Close()
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st.Mode().Perm(), err)
	}
}

func TestSiblingReaderHelperProcess(t *testing.T) {
	path := os.Getenv(siblingReaderHelperEnv)
	if path == "" {
		return
	}
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5})
	if err == nil {
		var n int64
		err = conn.QueryRowContext(context.Background(), "SELECT count(*) FROM sessions").Scan(&n)
		conn.Close()
	}
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestWritableStateDBKeepsLocksAcrossSecondOpen(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX fcntl locks")
	}
	path := filepath.Join(t.TempDir(), "state.db")
	first := mustSessionDB(t, path, false)
	defer first.Close()
	second := mustSessionDB(t, path, false)
	defer second.Close()
	if holders := IterDeletedSQLiteSidecarHolders(path); len(holders) != 0 {
		t.Fatalf("deleted sidecars after second open %v", holders)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSiblingReaderHelperProcess$")
	cmd.Env = append(os.Environ(), siblingReaderHelperEnv+"="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sibling reader failed: %v %s", err, out)
	}
	if holders := IterDeletedSQLiteSidecarHolders(path); len(holders) != 0 {
		t.Fatalf("a second SessionDB open or a sibling reader unlinked the live WAL/SHM inodes %v", holders)
	}
}

func TestFailedWritableOpenDoesNotLeakTrackedConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	var opened []*TrackedConn
	savedConnect, savedInit := sessionConnectTracked, sessionInitSchema
	defer func() { sessionConnectTracked, sessionInitSchema = savedConnect, savedInit }()
	sessionConnectTracked = func(p string, opts ...TrackedOption) (*TrackedConn, error) {
		conn, err := savedConnect(p, opts...)
		if conn != nil {
			opened = append(opened, conn)
		}
		return conn, err
	}
	sessionInitSchema = func(context.Context, *SessionDB) error {
		return &pyConvError{"RuntimeError", "schema init failed"}
	}
	defer func() {
		for _, c := range opened {
			c.Close()
		}
	}()
	db, err := NewSessionDB(context.Background(), path, false)
	if err == nil {
		db.Close()
		t.Fatal("open succeeded")
	}
	if !strings.Contains(err.Error(), "schema init failed") {
		t.Fatal(err)
	}
	if HasLiveConnection(path) {
		t.Fatal("failed open leaked a tracked connection")
	}
}

func TestFailedWALReadOpenDoesNotLeakTrackedConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := mustSessionDB(t, path, false)
	defer db.Close()
	savedPragmas := sessionApplyDatabasePragmas
	sessionApplyDatabasePragmas = func(context.Context, SQLiteConn, string) error {
		return &pyConvError{"RuntimeError", "read setup failed"}
	}
	before := liveConnectionsSnapshot()
	db.walActive.Store(true)
	conn, err := db.getReadConn(context.Background())
	sessionApplyDatabasePragmas = savedPragmas
	if conn != nil || err == nil || !strings.Contains(err.Error(), "read setup failed") {
		t.Fatalf("read conn %v err %v", conn, err)
	}
	if after := liveConnectionsSnapshot(); !maps.Equal(before, after) {
		t.Fatalf("live connections %v -> %v", before, after)
	}
}

func TestReadOnlyCloseNeverRequestsWALCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	w := mustSessionDB(t, path, false)
	seedSession(t, w, "s1")
	w.Close()
	ro := mustSessionDB(t, path, true)
	rec := traceWriter(ro)
	ro.Close()
	for _, q := range rec.sqls {
		if strings.Contains(strings.ToLower(q), "wal_checkpoint") {
			t.Fatalf("read only close ran %q", q)
		}
	}
}

func TestWritableCloseUsesPassiveCheckpoint(t *testing.T) {
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	rec := traceWriter(db)
	db.Close()
	passive := false
	for _, q := range rec.sqls {
		norm := strings.Join(strings.Fields(strings.ToLower(q)), " ")
		if norm == "pragma wal_checkpoint(truncate)" {
			t.Fatal("close requested a TRUNCATE checkpoint")
		}
		passive = passive || norm == "pragma wal_checkpoint(passive)"
	}
	if !passive {
		t.Fatalf("close never requested a PASSIVE checkpoint %v", rec.sqls)
	}
}

// Session ids of search hits
func hitSessionIDs(hits []*pyjson.Object) []string {
	var out []string
	for _, h := range hits {
		v, _ := h.Get("session_id")
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func TestReadOnlyConnectionKeepsFTSSearchAvailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	w := mustSessionDB(t, path, false)
	if err := w.CreateSession(NewSession{ID: "fts-read-only", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.AppendMessage("fts-read-only", &Message{Role: "user", Content: "readonlywoodpecker 大别山项目"}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	ro := mustSessionDB(t, path, true)
	defer ro.Close()
	ctx := context.Background()
	base, err := ro.sessionSearch.SearchMessages(ctx, "readonlywoodpecker", SearchMessagesOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	trigram, err := ro.sessionSearch.SearchMessages(ctx, "大别山", SearchMessagesOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(hitSessionIDs(base), ","); got != "fts-read-only" {
		t.Fatalf("base matches %q", got)
	}
	if got := strings.Join(hitSessionIDs(trigram), ","); got != "fts-read-only" {
		t.Fatalf("trigram matches %q", got)
	}
}

func TestFailedReadOnlyOpenDoesNotLeakTrackedConnection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	w := mustSessionDB(t, path, false)
	seedSession(t, w, "s1", "leak probe")
	w.Close()
	raw := openRaw(t, path)
	ctx := context.Background()
	execAll(t, raw, "PRAGMA writable_schema=ON")
	var typ, name, tbl, sqlText string
	var rootpage int64
	if err := raw.QueryRowContext(ctx, "SELECT type,name,tbl_name,rootpage,sql FROM sqlite_master WHERE name='messages_fts'").Scan(&typ, &name, &tbl, &rootpage, &sqlText); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "INSERT INTO sqlite_master (type,name,tbl_name,rootpage,sql) VALUES (?,?,?,?,?)", typ, name, tbl, rootpage, sqlText); err != nil {
		t.Fatal(err)
	}
	execAll(t, raw, "PRAGMA writable_schema=OFF")
	raw.Close()
	ro, err := NewSessionDB(ctx, path, true)
	if err == nil {
		ro.Close()
		t.Fatal("malformed store opened read only")
	}
	if !isDatabaseError(err) {
		t.Fatalf("refusal %s: %v", pyExceptionClassName(err), err)
	}
	if HasLiveConnection(path) {
		t.Fatal("failed read only open leaked a tracked connection")
	}
	mustSessionDB(t, path, false).Close()
	if backups, _ := filepath.Glob(filepath.Join(dir, "*malformed-backup*")); len(backups) == 0 {
		t.Fatal("the writable heal took no forensic backup")
	}
}

func TestReadOnlyOpenRetriesTransientWALIOErr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	w := mustSessionDB(t, path, false)
	seedSession(t, w, "wal-race")
	w.Close()
	savedConnect, savedBackoff := sessionConnectTracked, readOnlyIOErrRetryBackoffS
	defer func() { sessionConnectTracked, readOnlyIOErrRetryBackoffS = savedConnect, savedBackoff }()
	attempts := 0
	sessionConnectTracked = func(p string, opts ...TrackedOption) (*TrackedConn, error) {
		attempts++
		if attempts == 1 {
			return nil, &SQLiteError{Class: SQLiteOperationalError, Msg: "disk I/O error"}
		}
		return savedConnect(p, opts...)
	}
	readOnlyIOErrRetryBackoffS = 0
	ro := mustSessionDB(t, path, true)
	enabled := ro.ftsEnabled.Load()
	_, serr := ro.sessionSearch.SearchMessages(context.Background(), "wal-race", SearchMessagesOptions{Limit: 20})
	ro.Close()
	if !enabled || serr != nil {
		t.Fatalf("fts enabled %v search err %v", enabled, serr)
	}
	if attempts < 2 {
		t.Fatal("the transient IOERR must be retried")
	}
	if HasLiveConnection(path) {
		t.Fatal("leaked connection")
	}
}

func TestReadOnlyOpenExhaustsRetryBudgetForPersistentIOErr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	w := mustSessionDB(t, path, false)
	seedSession(t, w, "broken-disk")
	w.Close()
	savedConnect, savedBackoff := sessionConnectTracked, readOnlyIOErrRetryBackoffS
	defer func() { sessionConnectTracked, readOnlyIOErrRetryBackoffS = savedConnect, savedBackoff }()
	attempts := 0
	sessionConnectTracked = func(string, ...TrackedOption) (*TrackedConn, error) {
		attempts++
		return nil, &SQLiteError{Class: SQLiteOperationalError, Msg: "disk I/O error"}
	}
	readOnlyIOErrRetryBackoffS = 0
	_, err := NewSessionDB(context.Background(), path, true)
	if !isOperationalError(err) || !strings.Contains(err.Error(), "disk I/O error") {
		t.Fatalf("err %v", err)
	}
	if attempts != readOnlyIOErrRetryAttempts+1 {
		t.Fatalf("attempts %d", attempts)
	}
}

func TestGetMetaMissingReturnsNone(t *testing.T) {
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	defer db.Close()
	if _, ok, err := db.GetMeta(context.Background(), "nonexistent"); ok || err != nil {
		t.Fatalf("ok %v err %v", ok, err)
	}
}

func TestSetThenGetMeta(t *testing.T) {
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	defer db.Close()
	ctx := context.Background()
	if err := db.SetMeta(ctx, "foo", "bar"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := db.GetMeta(ctx, "foo"); !ok || v != "bar" || err != nil {
		t.Fatalf("%q %v %v", v, ok, err)
	}
}
