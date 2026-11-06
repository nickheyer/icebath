package kanban

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
)

// Holds the cross process init lock from another goroutine until the returned release runs
func holdInitLock(t *testing.T, dbPath string) func() {
	t.Helper()
	holding, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		rel, err := crossProcessInitLock(dbPath)
		if err != nil {
			t.Error(err)
			close(holding)
			return
		}
		close(holding)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		rel()
	}()
	select {
	case <-holding:
	case <-time.After(5 * time.Second):
		t.Fatal("holder never acquired the lock")
	}
	return func() { close(release); <-done }
}

func TestInitializedPathConnectSkipsInitLock(t *testing.T) {
	setupKanbanHome(t)
	dbPath := mustDBPath(t, BoardArg("default"))
	mustConnect(t, "", nil).Close()
	if !isInitialized(dbPath) {
		t.Fatal("path not initialized")
	}
	release := holdInitLock(t, dbPath)
	defer release()
	start := time.Now()
	mustConnect(t, "", nil).Close()
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("fast-path connect blocked on the init lock (%v)", elapsed)
	}
}

func TestFirstInitConnectIsBoundedWhenLockHeld(t *testing.T) {
	setupKanbanHome(t)
	old := initLockTimeout
	initLockTimeout = 600 * time.Millisecond
	t.Cleanup(func() { initLockTimeout = old })
	dbPath := mustDBPath(t, BoardArg("default"))
	release := holdInitLock(t, dbPath)
	defer release()
	start := time.Now()
	mustConnect(t, "", nil).Close()
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond || elapsed >= 8*time.Second {
		t.Fatalf("expected bounded ~0.6s acquire, got %v", elapsed)
	}
	if !isInitialized(dbPath) {
		t.Fatal("path not initialized")
	}
}

// Connection replaying scripted boundary outcomes keyed by the uppercased first SQL word
type fakeBoundaryConn struct {
	mu     sync.Mutex
	script map[string][]error
	calls  []string
}

func (f *fakeBoundaryConn) ExecContext(_ context.Context, query string, _ ...any) (sql.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, query)
	key := strings.ToUpper(strings.Fields(query)[0])
	if outcomes := f.script[key]; len(outcomes) > 0 {
		f.script[key] = outcomes[1:]
		if outcomes[0] != nil {
			return nil, outcomes[0]
		}
	}
	return nil, nil
}

func (f *fakeBoundaryConn) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, &state.SQLiteError{Class: state.SQLiteOperationalError, Msg: "fake connection runs no queries"}
}

func (f *fakeBoundaryConn) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }
func (f *fakeBoundaryConn) Close() error                                             { return nil }

func (f *fakeBoundaryConn) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(c)), prefix) {
			n++
		}
	}
	return n
}

func busyErr() error {
	return &state.SQLiteError{Class: state.SQLiteOperationalError, Msg: "database is locked"}
}

func TestRetrySleepRespectsFloor(t *testing.T) {
	var slept []time.Duration
	old := busySleep
	busySleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { busySleep = old })
	conn := &fakeBoundaryConn{script: map[string][]error{"BEGIN": {busyErr(), busyErr(), nil}}}
	if err := WriteTxn(context.Background(), conn, false, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(slept) == 0 {
		t.Fatal("no retry sleep")
	}
	for _, s := range slept {
		if s < busyRetryMin || s > busyRetryMax {
			t.Fatalf("sleep %v outside [%v, %v]", s, busyRetryMin, busyRetryMax)
		}
	}
}

func TestTransientBusyAtBeginIsAbsorbed(t *testing.T) {
	conn := &fakeBoundaryConn{script: map[string][]error{"BEGIN": {busyErr(), nil}}}
	if err := WriteTxn(context.Background(), conn, false, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if conn.count("BEGIN") != 2 || conn.count("COMMIT") != 1 {
		t.Fatalf("calls %v", conn.calls)
	}
}

func TestPersistentBusyAtCommitRollsBack(t *testing.T) {
	old := busySleep
	busySleep = func(time.Duration) {}
	t.Cleanup(func() { busySleep = old })
	errs := make([]error, 50)
	for i := range errs {
		errs[i] = busyErr()
	}
	conn := &fakeBoundaryConn{script: map[string][]error{"COMMIT": errs}}
	err := WriteTxn(context.Background(), conn, false, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatal(err)
	}
	if conn.count("ROLLBACK") != 1 {
		t.Fatalf("calls %v", conn.calls)
	}
}

func TestWriteTxnRefusesUnrequestedNestingAndNestsSavepoints(t *testing.T) {
	setupKanbanHome(t)
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	ctx := context.Background()
	err := WriteTxn(ctx, conn, false, func() error {
		return WriteTxn(ctx, conn, false, func() error { return nil })
	})
	var rt *RuntimeError
	if !errors.As(err, &rt) || !strings.Contains(rt.Msg, "already inside a transaction") {
		t.Fatal(err)
	}
	err = WriteTxn(ctx, conn, false, func() error {
		mustExec(t, conn, "INSERT INTO tasks (id, title, status, created_at) VALUES ('t-a', 'A', 'ready', 1)")
		inner := WriteTxn(ctx, conn, true, func() error {
			mustExec(t, conn, "INSERT INTO tasks (id, title, status, created_at) VALUES ('t-b', 'B', 'ready', 1)")
			return errors.New("inner failure")
		})
		if inner == nil || inner.Error() != "inner failure" {
			t.Fatal(inner)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := mustRows(t, conn, "SELECT id FROM tasks ORDER BY id")
	if len(ids) != 1 || ids[0].Get("id") != "t-a" {
		t.Fatalf("rows %v", ids)
	}
}

func TestConnectSucceedsUnderTestHome(t *testing.T) {
	h := filepath.Join(t.TempDir(), "icebath_home")
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	if p := mustDBPath(t, nil); !strings.HasPrefix(p, h) {
		t.Fatalf("%s not under %s", p, h)
	}
}

func TestConnectRaisesWhenKanbanHomeIsRealRoot(t *testing.T) {
	oldHome, oldPath := kanbanHomeFn, kanbanDBPathFn
	kanbanHomeFn = func() string { return realKanbanRoot }
	kanbanDBPathFn = func(context.Context, *string) (string, error) {
		return filepath.Join(realKanbanRoot, "kanban.db"), nil
	}
	t.Cleanup(func() { kanbanHomeFn, kanbanDBPathFn = oldHome, oldPath })
	if _, err := Connect(context.Background(), "", nil); err == nil || !strings.Contains(err.Error(), "kanban_write_guard") {
		t.Fatal(err)
	}
}

func TestConnectRaisesForExplicitDBPathUnderRealRoot(t *testing.T) {
	if _, err := Connect(context.Background(), filepath.Join(realKanbanRoot, "kanban.db"), nil); err == nil ||
		!strings.Contains(err.Error(), "kanban_write_guard") {
		t.Fatal(err)
	}
}

func TestDispatchLockIsBoardScoped(t *testing.T) {
	setupKanbanHome(t)
	if _, err := InitDB(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
	dbDefault := mustDBPath(t, BoardArg("default"))
	dbOther := filepath.Join(filepath.Dir(dbDefault), "other-board-kanban.db")
	heldA, releaseA := dispatchTickLock(dbDefault)
	defer releaseA()
	if !heldA {
		t.Fatal("default board lock not acquired")
	}
	heldB, releaseB := dispatchTickLock(dbOther)
	defer releaseB()
	if !heldB {
		t.Fatal("a lock on a different board must be independent")
	}
	heldAgain, releaseAgain := dispatchTickLock(dbDefault)
	defer releaseAgain()
	if heldAgain {
		t.Fatal("a second handle acquired a held board lock")
	}
}

// Kanban database with the pre AUTOINCREMENT text key shape for the four rebuilt tables
func makeLegacyDB(t *testing.T, path string) {
	conn := rawSQLite(t, path)
	defer conn.Close()
	mustExec(t, conn, SchemaSQL, `
        DROP TABLE task_events;
        DROP TABLE task_comments;
        DROP TABLE task_runs;
        DROP TABLE kanban_notify_subs;
        CREATE TABLE task_comments (id TEXT PRIMARY KEY, task_id TEXT NOT NULL,
            author TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL);
        CREATE TABLE task_events (id TEXT PRIMARY KEY, task_id TEXT NOT NULL,
            kind TEXT NOT NULL, payload TEXT, created_at INTEGER NOT NULL);
        CREATE TABLE task_runs (id TEXT PRIMARY KEY, task_id TEXT NOT NULL,
            profile TEXT, status TEXT NOT NULL, started_at INTEGER NOT NULL);
        CREATE TABLE kanban_notify_subs (task_id TEXT NOT NULL, platform TEXT NOT NULL,
            chat_id TEXT NOT NULL, thread_id TEXT NOT NULL DEFAULT '', user_id TEXT,
            created_at INTEGER NOT NULL, last_event_id TEXT,
            PRIMARY KEY (task_id, platform, chat_id, thread_id));`,
		"INSERT INTO tasks (id, title, status, created_at) VALUES ('task-1', 'T', 'done', 1000)",
		"INSERT INTO task_comments VALUES ('c-1', 'task-1', 'agent', 'hi', 1500)",
		"INSERT INTO task_events VALUES ('e-1', 'task-1', 'completed', NULL, 2000)",
		"INSERT INTO task_events VALUES ('e-2', 'task-1', 'blocked', NULL, 2100)",
		"INSERT INTO task_runs VALUES ('r-1', 'task-1', 'default', 'done', 1000)",
		"INSERT INTO kanban_notify_subs (task_id, platform, chat_id, created_at, last_event_id) VALUES ('task-1', 'telegram', '123', 1000, 'e-1')")
}

// Legacy board database path under a fresh home
func setupLegacyHome(t *testing.T) string {
	setupKanbanHome(t)
	t.Setenv("ICEBATH_KANBAN_HOME", "")
	p := mustDBPath(t, BoardArg("legacy"))
	os.MkdirAll(filepath.Dir(p), 0o755)
	discardInitialized(t, p)
	return p
}

// table_info rows keyed by column name
func tableInfo(t *testing.T, conn state.SQLiteConn, table string) map[string]*Row {
	out := map[string]*Row{}
	for _, r := range mustRows(t, conn, "PRAGMA table_info("+table+")") {
		out[r.Get("name").(string)] = r
	}
	return out
}

func TestLegacyTextPKTablesRebuiltToIntegerAutoincrement(t *testing.T) {
	dbPath := setupLegacyHome(t)
	makeLegacyDB(t, dbPath)
	conn := mustConnect(t, dbPath, nil)
	defer conn.Close()
	for _, table := range []string{"task_events", "task_comments", "task_runs"} {
		id := tableInfo(t, conn, table)["id"]
		if strings.ToUpper(id.Get("type").(string)) != "INTEGER" || id.Get("pk") != int64(1) {
			t.Fatalf("%s id %v", table, id.Vals)
		}
	}
	lei := tableInfo(t, conn, "kanban_notify_subs")
	if strings.ToUpper(lei["last_event_id"].Get("type").(string)) != "INTEGER" || lei["delivery_metadata"] == nil {
		t.Fatal("notify subs not rebuilt")
	}
	if n := len(mustRows(t, conn, "SELECT * FROM task_events")); n != 2 {
		t.Fatalf("events %d", n)
	}
	if b := mustRows(t, conn, "SELECT body FROM task_comments")[0].Get("body"); b != "hi" {
		t.Fatal(b)
	}
	if n := len(mustRows(t, conn, "SELECT * FROM task_runs")); n != 1 {
		t.Fatalf("runs %d", n)
	}
	if v := mustRows(t, conn, "SELECT last_event_id FROM kanban_notify_subs")[0].Get("last_event_id"); v != int64(0) {
		t.Fatal(v)
	}
	indexes := map[string]bool{}
	for _, r := range mustRows(t, conn, "SELECT name FROM sqlite_master WHERE type='index'") {
		indexes[r.Vals[0].(string)] = true
	}
	for _, name := range []string{"idx_events_task", "idx_events_run", "idx_comments_task", "idx_runs_task", "idx_runs_status", "idx_notify_task"} {
		if !indexes[name] {
			t.Fatalf("missing index %s", name)
		}
	}
	mustExec(t, conn, "INSERT INTO task_events (task_id, kind, created_at) VALUES ('task-1', 'completed', 3000)")
	if id, ok := mustRows(t, conn, "SELECT id FROM task_events ORDER BY id DESC LIMIT 1")[0].Get("id").(int64); !ok || id < 1 {
		t.Fatal("new event id not an integer")
	}
}

func TestMigrationIsIdempotent(t *testing.T) {
	dbPath := setupLegacyHome(t)
	makeLegacyDB(t, dbPath)
	mustConnect(t, dbPath, nil).Close()
	discardInitialized(t, dbPath)
	conn := mustConnect(t, dbPath, nil)
	defer conn.Close()
	if strings.ToUpper(tableInfo(t, conn, "task_events")["id"].Get("type").(string)) != "INTEGER" {
		t.Fatal("id not integer")
	}
	if n := len(mustRows(t, conn, "SELECT * FROM task_events")); n != 2 {
		t.Fatalf("events %d", n)
	}
}

// Default board database path under a fresh home
func defaultBoardDB(t *testing.T) string {
	setupKanbanHome(t)
	t.Setenv("ICEBATH_KANBAN_HOME", "")
	p := mustDBPath(t, BoardArg("default"))
	os.MkdirAll(filepath.Dir(p), 0o755)
	discardInitialized(t, p)
	return p
}

// Table names in a database read through a plain connection
func tablesOf(t *testing.T, path string) map[string]bool {
	conn := rawSQLite(t, path)
	defer conn.Close()
	out := map[string]bool{}
	for _, r := range mustRows(t, conn, "SELECT name FROM sqlite_master WHERE type='table'") {
		out[r.Vals[0].(string)] = true
	}
	return out
}

func TestConnectReinitializesSchemaWhenDBFileVanished(t *testing.T) {
	dbPath := defaultBoardDB(t)
	ctx := context.Background()
	if err := ConnectClosing(ctx, dbPath, nil, func(conn state.SQLiteConn) error {
		mustExec(t, conn, "INSERT INTO tasks (id, title, status, created_at) VALUES ('t-1', 'T', 'ready', 1000)")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !isInitialized(dbPath) {
		t.Fatal("not initialized")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(dbPath + suffix)
	}
	if err := ConnectClosing(ctx, dbPath, nil, func(conn state.SQLiteConn) error {
		if n := mustRows(t, conn, "SELECT COUNT(*) FROM tasks")[0].Vals[0]; n != int64(0) {
			t.Fatal(n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !tablesOf(t, dbPath)["tasks"] {
		t.Fatal("schema not recreated")
	}
}

func TestConnectReinitializesSchemaWhenDBReplacedByEmptyFile(t *testing.T) {
	dbPath := defaultBoardDB(t)
	ctx := context.Background()
	if err := ConnectClosing(ctx, dbPath, nil, func(state.SQLiteConn) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(dbPath + suffix)
	}
	rawSQLite(t, dbPath).Close()
	if tablesOf(t, dbPath)["tasks"] {
		t.Fatal("tasks survived")
	}
	if err := ConnectClosing(ctx, dbPath, nil, func(conn state.SQLiteConn) error {
		mustExec(t, conn, "INSERT INTO tasks (id, title, status, created_at) VALUES ('t-2', 'T', 'ready', 1000)")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !tablesOf(t, dbPath)["tasks"] {
		t.Fatal("schema not recreated")
	}
}

func TestHealthyFastPathStaysLockFree(t *testing.T) {
	dbPath := defaultBoardDB(t)
	ctx := context.Background()
	noop := func(state.SQLiteConn) error { return nil }
	if err := ConnectClosing(ctx, dbPath, nil, noop); err != nil {
		t.Fatal(err)
	}
	var locks []string
	old := crossProcessInitLockFn
	crossProcessInitLockFn = func(path string) (func(), error) {
		locks = append(locks, path)
		return old(path)
	}
	t.Cleanup(func() { crossProcessInitLockFn = old })
	if err := ConnectClosing(ctx, dbPath, nil, noop); err != nil {
		t.Fatal(err)
	}
	if len(locks) != 0 {
		t.Fatalf("locks %v", locks)
	}
	os.Remove(dbPath)
	if err := ConnectClosing(ctx, dbPath, nil, noop); err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 {
		t.Fatalf("locks %v", locks)
	}
}

// Page corrupt database bytes behind a valid header
func writePageCorruptDB(t *testing.T, path string) {
	header := []byte("SQLite format 3\x00\x10\x00\x02\x02\x00\x40\x20\x20\x00\x00\x00\x0c\x00\x00\x23\x46\x00\x00\x00\x00")
	header = append(header, make([]byte, 100-len(header))...)
	blob := append(header, []byte(strings.Repeat("definitely not a valid sqlite page \x00\x01\x02\x03", 64))...)
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	discardInitialized(t, path)
}

func TestCorruptBackupRetentionCapPrunesOldest(t *testing.T) {
	old := corruptBackupRetention
	corruptBackupRetention = 3
	t.Cleanup(func() { corruptBackupRetention = old })
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "kanban.db")
	writePageCorruptDB(t, dbPath)
	var minted []string
	for i := range 8 {
		fh, err := os.OpenFile(dbPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		fh.WriteAt([]byte(strings.Repeat(string(rune(i)), 16)), 200)
		fh.Close()
		discardInitialized(t, dbPath)
		_, err = Connect(context.Background(), dbPath, nil)
		var corrupt *KanbanDBCorruptError
		if !errors.As(err, &corrupt) || corrupt.BackupPath == "" || !pathExists(corrupt.BackupPath) {
			t.Fatalf("round %d: %v", i, err)
		}
		minted = append(minted, corrupt.BackupPath)
	}
	remaining, _ := pypath.Glob(tmp, "kanban.db.corrupt.*.bak")
	if len(remaining) != 3 || !slices.Contains(remaining, minted[len(minted)-1]) {
		t.Fatalf("remaining %v", remaining)
	}
}

func TestValidateHeaderNamesTLSRecordSignature(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "kanban.db")
	os.WriteFile(p, append([]byte("SQLit\x16\x03\x01\x00\x10"), make([]byte, 40)...), 0o644)
	err := validateSQLiteHeader(p)
	if err == nil || !strings.Contains(err.Error(), "file is not a database: invalid SQLite header for "+p+
		" (TLS record header detected at byte offset 5); first_32=53 51 4c 69 74 16 03 01 00 10 00") {
		t.Fatal(err)
	}
	empty := filepath.Join(tmp, "empty.db")
	os.WriteFile(empty, nil, 0o644)
	if err := validateSQLiteHeader(empty); err != nil {
		t.Fatal(err)
	}
}

func TestRepairableIndexNamesRequiresEveryMessageIndexScoped(t *testing.T) {
	got := repairableIndexNames([]string{"wrong # of entries in index idx_a", "row 3 missing from index idx_a", "row 4 missing from index idx_b"})
	if !slices.Equal(got, []string{"idx_a", "idx_b"}) {
		t.Fatal(got)
	}
	if repairableIndexNames([]string{"wrong # of entries in index idx_a", "Page 4 is never used"}) != nil || repairableIndexNames(nil) != nil {
		t.Fatal("non index message accepted")
	}
}

func TestRepairDBReportsMissingOkAndCorrupt(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	missing := filepath.Join(tmp, "none.db")
	if r, err := RepairDB(ctx, missing, nil); err != nil || r.Status != "missing" {
		t.Fatal(r, err)
	}
	healthy := filepath.Join(tmp, "kanban.db")
	if _, err := InitDB(ctx, healthy, nil); err != nil {
		t.Fatal(err)
	}
	if r, err := RepairDB(ctx, healthy, nil); err != nil || r.Status != "ok" || !integrityMessagesOK(r.Messages) {
		t.Fatal(r, err)
	}
	corrupt := filepath.Join(tmp, "corrupt.db")
	writePageCorruptDB(t, corrupt)
	r, err := RepairDB(ctx, corrupt, nil)
	if err != nil || r.Status != "corrupt" || r.BackupPath == "" {
		t.Fatal(r, err)
	}
}
