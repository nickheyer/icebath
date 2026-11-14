package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const intruderHelperEnv = "ICEBATH_INTRUDER_HELPER_DB"

// Child half of the lock tests, printing ACQUIRED when it commits into the database and BLOCKED when locked out
func TestIntruderHelperProcess(t *testing.T) {
	path := os.Getenv(intruderHelperEnv)
	if path == "" {
		return
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		os.Exit(2)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		os.Exit(3)
	}
	for _, stmt := range []string{"PRAGMA busy_timeout=0", "BEGIN IMMEDIATE", "INSERT INTO t(v) VALUES ('intruder')", "COMMIT"} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			fmt.Println("BLOCKED")
			os.Exit(0)
		}
	}
	fmt.Println("ACQUIRED")
	os.Exit(0)
}

// True when a separate process managed to write to a locked database
func externalWriterCanBreakIn(t *testing.T, dbPath string) bool {
	cmd := exec.Command(os.Args[0], "-test.run=^TestIntruderHelperProcess$")
	cmd.Env = append(os.Environ(), intruderHelperEnv+"="+dbPath)
	out, err := cmd.Output()
	if err != nil {
		t.Errorf("intruder failed %v %s", err, out)
	}
	return strings.Contains(string(out), "ACQUIRED")
}

// Creates table t with 200 rows in one transaction under the journal mode
func makeSafeReadDB(t *testing.T, path, journalMode string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	for _, stmt := range []string{"PRAGMA journal_mode=" + journalMode, "CREATE TABLE t(v TEXT)", "BEGIN"} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 200 {
		if _, err := conn.ExecContext(ctx, "INSERT INTO t(v) VALUES (?)", fmt.Sprintf("row%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
}

// Clears the live registry after the test so counts never leak across tests
func cleanLiveRegistry(t *testing.T) {
	t.Cleanup(func() {
		liveMu.Lock()
		clear(liveConnections)
		liveMu.Unlock()
	})
}

func mustConnectTracked(t *testing.T, path string, opts ...TrackedOption) *TrackedConn {
	t.Helper()
	conn, err := ConnectTracked(path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestPreopenReadRefusedWhileConnectionIsLive(t *testing.T) {
	cleanLiveRegistry(t)
	db := filepath.Join(t.TempDir(), "state.db")
	makeSafeReadDB(t, db, "WAL")
	if HasLiveConnection(db) {
		t.Fatal("live before any connection")
	}
	head, ok := ReadHeaderBytesPreopen(db, 16, false)
	if !ok || !bytes.Equal(head, []byte("SQLite format 3\x00")) {
		t.Fatalf("header %q %v", head, ok)
	}
	TrackConnection(db)
	if !HasLiveConnection(db) {
		t.Fatal("tracked path not live")
	}
	if _, ok := ReadHeaderBytesPreopen(db, 16, false); ok {
		t.Fatal("probe allowed on a live database")
	}
	if _, ok := ReadHeaderBytesPreopen(db, 16, true); !ok {
		t.Fatal("an explicit override stays available for offline artifacts")
	}
	UntrackConnection(db)
	if HasLiveConnection(db) {
		t.Fatal("still live after untrack")
	}
	if _, ok := ReadHeaderBytesPreopen(db, 16, false); !ok {
		t.Fatal("probe refused after untrack")
	}
}

func TestTrackingRegistryDoesNotLeakAcrossClosePaths(t *testing.T) {
	cleanLiveRegistry(t)
	db := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	boot := mustConnectTracked(t, db)
	if _, err := boot.ExecContext(ctx, "CREATE TABLE t(v TEXT)"); err != nil {
		t.Fatal(err)
	}
	boot.Close()
	if HasLiveConnection(db) {
		t.Fatal("plain close leaked")
	}
	func() {
		conn := mustConnectTracked(t, db)
		defer conn.Close()
		if !HasLiveConnection(db) {
			t.Fatal("deferred close scope not live")
		}
	}()
	if HasLiveConnection(db) {
		t.Fatal("deferred close leaked")
	}
	conn := mustConnectTracked(t, db)
	for _, stmt := range []string{"BEGIN", "INSERT INTO t(v) VALUES ('x')", "COMMIT"} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if !HasLiveConnection(db) {
		t.Fatal("transaction scope must not untrack")
	}
	conn.Close()
	dup := mustConnectTracked(t, db)
	if err := dup.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dup.Close(); err != nil {
		t.Fatalf("second close %v", err)
	}
	if HasLiveConnection(db) {
		t.Fatal("double close under counted into a leak")
	}
	first, second := mustConnectTracked(t, db), mustConnectTracked(t, db)
	first.Close()
	if !HasLiveConnection(db) {
		t.Fatal("nested lifetime released early")
	}
	second.Close()
	for range 100 {
		mustConnectTracked(t, db).Close()
	}
	if HasLiveConnection(db) {
		t.Fatal("churn drifted the counter")
	}
}

// Connection double whose close fails while armed, like a cross thread ProgrammingError
type controllableConn struct {
	SQLiteConn
	failClose bool
}

func (c *controllableConn) Close() error {
	if c.failClose {
		return errors.New("SQLite objects created in a thread can only be used in that same thread")
	}
	return c.SQLiteConn.Close()
}

func TestFailedCloseKeepsConnectionTracked(t *testing.T) {
	cleanLiveRegistry(t)
	db := filepath.Join(t.TempDir(), "state.db")
	makeSafeReadDB(t, db, "WAL")
	var inner *controllableConn
	conn := mustConnectTracked(t, db, WithConnectFn(func(name string, cfg SQLiteConfig) (SQLiteConn, error) {
		c, err := ConnectSQLite(name, cfg)
		inner = &controllableConn{SQLiteConn: c}
		return inner, err
	}))
	if !HasLiveConnection(db) {
		t.Fatal("custom opener connection untracked")
	}
	if _, ok := ReadHeaderBytesPreopen(db, 16, false); ok {
		t.Fatal("probe allowed on a live database")
	}
	inner.failClose = true
	if err := conn.Close(); err == nil {
		t.Fatal("close did not fail")
	}
	if !HasLiveConnection(db) {
		t.Fatal("failed close must leave the registry entry")
	}
	if _, ok := ReadHeaderBytesPreopen(db, 16, false); ok {
		t.Fatal("probe allowed after a failed close")
	}
	inner.failClose = false
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if HasLiveConnection(db) {
		t.Fatal("still live after the real close")
	}
	if _, ok := ReadHeaderBytesPreopen(db, 16, false); !ok {
		t.Fatal("probe refused after the real close")
	}
}

func TestProbeAndConnectDoNotRace(t *testing.T) {
	cleanLiveRegistry(t)
	db := filepath.Join(t.TempDir(), "state.db")
	makeSafeReadDB(t, db, "DELETE")
	insideRead, mayClose := make(chan struct{}), make(chan struct{})
	var insideOnce, closeOnce sync.Once
	letClose := func() { closeOnce.Do(func() { close(mayClose) }) }
	var mu sync.Mutex
	var failures []string
	fail := func(msg string) { mu.Lock(); failures = append(failures, msg); mu.Unlock() }
	saved := safeReadOpen
	t.Cleanup(func() { safeReadOpen = saved })
	safeReadOpen = func(name string) (*os.File, error) {
		f, err := os.Open(name)
		insideOnce.Do(func() { close(insideRead) })
		select {
		case <-mayClose:
		case <-time.After(2 * time.Second):
		}
		return f, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-insideRead:
		case <-time.After(10 * time.Second):
		}
		conn, err := ConnectTracked(db, WithTimeout(0.5))
		if err != nil {
			fail("connect failed: " + err.Error())
			letClose()
			return
		}
		defer conn.Close()
		ctx := context.Background()
		for _, stmt := range []string{"BEGIN IMMEDIATE", "INSERT INTO t(v) VALUES ('holder')"} {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				fail("holder transaction failed: " + err.Error())
				letClose()
				return
			}
		}
		letClose()
		if externalWriterCanBreakIn(t, db) {
			fail("external writer broke in: the probe's close() cancelled this connection's POSIX locks")
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			fail("holder transaction failed: " + err.Error())
		}
	}()
	ReadHeaderBytesPreopen(db, 16, false)
	letClose()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("writer never finished")
	}
	if len(failures) > 0 {
		t.Fatal(failures[0])
	}
}

func TestPageCountBytesMatchesOnDiskSize(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	makeSafeReadDB(t, db, "DELETE")
	conn, err := ConnectSQLite(db, SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	logical, ok := PageCountBytes(conn)
	st, _ := os.Stat(db)
	if !ok || logical != st.Size() {
		t.Fatalf("logical %d %v on disk %d", logical, ok, st.Size())
	}
	if matches, ok := FileLengthMatchesHeader(conn); !matches || !ok {
		t.Fatal("an intact file does not match its header")
	}
}

func TestFileLengthCheckNeverReportsTruncatedDBAsHealthy(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	makeSafeReadDB(t, db, "DELETE")
	conn, err := ConnectSQLite(db, SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	logical, ok := PageCountBytes(conn)
	if !ok {
		t.Fatal("no logical size")
	}
	if matches, ok := FileLengthMatchesHeader(conn); !matches || !ok {
		t.Fatal("an intact file does not match its header")
	}
	if err := os.Truncate(db, logical/2); err != nil {
		t.Fatal(err)
	}
	if matches, ok := FileLengthMatchesHeader(conn); matches && ok {
		t.Fatal("a torn file was reported healthy")
	}
}

func TestInMemoryConnectionsAreNotTrackedAndTrackingPathWins(t *testing.T) {
	cleanLiveRegistry(t)
	mem := mustConnectTracked(t, ":memory:")
	if _, ok := canonicalDBPath(mem); ok || mem.trackedPath != "" {
		t.Fatal("an in memory database has no file to probe")
	}
	if logical, ok := PageCountBytes(mem); !ok || logical != 0 {
		t.Fatalf("in memory page bytes %d %v", logical, ok)
	}
	if _, ok := FileLengthMatchesHeader(mem); ok {
		t.Fatal("an in memory database length check is inconclusive")
	}
	mem.Close()
	liveMu.Lock()
	n := len(liveConnections)
	liveMu.Unlock()
	if n != 0 {
		t.Fatalf("registry holds %d keys after an in memory connection", n)
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	makeSafeReadDB(t, db, "WAL")
	uri, err := ReadOnlyDBURI(db)
	if err != nil {
		t.Fatal(err)
	}
	ro := mustConnectTracked(t, uri, WithURI(), WithTrackingPath(db))
	if !HasLiveConnection(db) || !HasLiveConnection(db+"-wal") || !HasLiveConnection(db+"-shm") {
		t.Fatal("a read only URI connection is not tracked under its tracking path")
	}
	if _, err := ro.ExecContext(context.Background(), "INSERT INTO t(v) VALUES ('x')"); err == nil {
		t.Fatal("mode=ro was not honoured")
	}
	ro.Close()
	if HasLiveConnection(db) {
		t.Fatal("read only connection leaked")
	}
}

func TestConnectSQLiteKeepsLiteralFilenames(t *testing.T) {
	cleanLiveRegistry(t)
	dir := t.TempDir()
	t.Chdir(dir)
	names := []string{filepath.Join(dir, "odd?name #1%.db"), "file:literal.db", "rel?q.db"}
	if runtime.GOOS == "windows" {
		names = []string{filepath.Join(dir, "odd name #1%.db"), "rel #q.db"}
	}
	for _, name := range names {
		conn := mustConnectTracked(t, name)
		if _, err := conn.ExecContext(context.Background(), "CREATE TABLE x(y)"); err != nil {
			t.Fatal(err)
		}
		want := liveKey(name)
		if conn.trackedPath != want {
			t.Fatalf("%s tracked as %s want %s", name, conn.trackedPath, want)
		}
		conn.Close()
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("%s was not created literally: %v", name, err)
		}
	}
	if HasLiveConnection(filepath.Join(dir, "rel?q.db")) {
		t.Fatal("leaked")
	}
}

func TestConnectTrackedReusesATrackingConnection(t *testing.T) {
	cleanLiveRegistry(t)
	db := filepath.Join(t.TempDir(), "state.db")
	var made *TrackedConn
	conn := mustConnectTracked(t, db, WithConnectFn(func(name string, cfg SQLiteConfig) (SQLiteConn, error) {
		c, err := ConnectSQLite(name, cfg)
		made = &TrackedConn{SQLiteConn: c}
		return made, err
	}))
	if conn != made || conn.trackedPath != liveKey(db) {
		t.Fatal("a connection that already untracks on close must be reused, not wrapped")
	}
	conn.Close()
	if HasLiveConnection(db) {
		t.Fatal("leaked")
	}
	if _, err := ConnectTracked(filepath.Join(t.TempDir(), "missing", "x.db")); err == nil {
		t.Fatal("an open failure must propagate")
	}
	liveMu.Lock()
	n := len(liveConnections)
	liveMu.Unlock()
	if n != 0 {
		t.Fatal("a failed open registered a connection")
	}
}

func TestBusyTimeoutReachesTheConnection(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	for _, c := range []struct {
		timeout float64
		want    int64
	}{{5, 5000}, {0, 0}, {0.5, 500}, {1.2345, 1234}} {
		conn, err := ConnectSQLite(db, SQLiteConfig{Timeout: c.timeout})
		if err != nil {
			t.Fatal(err)
		}
		var got int64
		if err := conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&got); err != nil || got != c.want {
			t.Fatalf("timeout %v gave %d %v", c.timeout, got, err)
		}
		conn.Close()
	}
}

func TestHeaderProbeShapes(t *testing.T) {
	cleanLiveRegistry(t)
	logs := captureSlog(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	if err := os.WriteFile(db, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		length int
		want   string
	}{{0, ""}, {4, "0123"}, {100, "0123456789"}, {-1, "0123456789"}, {-7, "0123456789"}} {
		got, ok := ReadHeaderBytesPreopen(db, c.length, false)
		if !ok || string(got) != c.want {
			t.Fatalf("length %d read %q %v", c.length, got, ok)
		}
	}
	if _, ok := ReadHeaderBytesPreopen(dir, 0, false); ok {
		t.Fatal("a directory is not a readable file")
	}
	if _, ok := ReadHeaderBytesPreopen(filepath.Join(dir, "missing"), 16, false); ok {
		t.Fatal("a missing file has no header")
	}
	TrackConnection(db)
	defer UntrackConnection(db)
	if _, ok := ReadHeaderBytesPreopen(db+"-wal", 16, false); ok {
		t.Fatal("a sidecar of a live database was probed")
	}
	if !strings.Contains(logs.String(), "refusing byte-level read of "+db+"-wal: a live connection exists in this process and close() would cancel its POSIX locks") {
		t.Fatal(logs.String())
	}
	err := OfflineFileAccess(db+"-shm", "copy", func() { t.Fatal("ran while live") })
	var live *LiveConnectionError
	if !errors.As(err, &live) || !strings.HasPrefix(err.Error(), "Refusing to copy "+db+"-shm: a connection to its main database "+liveKey(db)+" is still open") {
		t.Fatalf("%v", err)
	}
	err = OfflineFileAccess(db, "", func() { t.Fatal("ran while live") })
	if err == nil || !strings.HasPrefix(err.Error(), "Refusing to read "+db+": a connection to it is still open") {
		t.Fatalf("%v", err)
	}
}

func TestPageCountBytesLogsUnreadablePragmas(t *testing.T) {
	logs := captureSlog(t)
	conn, err := ConnectSQLite(filepath.Join(t.TempDir(), "state.db"), SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, ok := PageCountBytes(conn); ok {
		t.Fatal("a closed connection read its pragmas")
	}
	if !strings.Contains(logs.String(), "page_count/page_size unavailable: ") {
		t.Fatal(logs.String())
	}
	if _, ok := FileLengthMatchesHeader(conn); ok {
		t.Fatal("a closed connection length check is inconclusive")
	}
}

func TestOpenerReturningNoConnectionIsRefused(t *testing.T) {
	cleanLiveRegistry(t)
	db := filepath.Join(t.TempDir(), "state.db")
	none := WithConnectFn(func(string, SQLiteConfig) (SQLiteConn, error) { return nil, nil })
	typedNone := WithConnectFn(func(string, SQLiteConfig) (SQLiteConn, error) { return (*sqliteConn)(nil), nil })
	for _, opener := range []TrackedOption{none, typedNone} {
		_, err := ConnectTracked(db, opener, WithTrackingPath(db))
		var untrackable *UntrackableConnectionError
		want := "connection to " + liveKey(db) + " uses factory NoneType, which cannot release its tracking entry on close; " +
			"byte-probe safety for this database would be silently lost"
		var cause *pyConvError
		if !errors.As(err, &untrackable) || err.Error() != want || !errors.As(err, &cause) || cause.Class != "TypeError" {
			t.Fatalf("%v", err)
		}
		_, err = ConnectTracked(db, opener)
		if !errors.As(err, &cause) || cause.Class != "AttributeError" {
			t.Fatalf("%v", err)
		}
	}
	if HasLiveConnection(db) {
		t.Fatal("a refused connection was registered")
	}
}
