package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sqlite3 "modernc.org/sqlite/lib"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
)

// Opens a test connection to a database file with extra DSN pragmas, closed with the test
func testConn(t *testing.T, path, query string) *sqliteConn {
	t.Helper()
	uri, err := pypath.AsURI(path)
	if err != nil {
		t.Fatal(err)
	}
	if query != "" {
		uri += "?" + query
	}
	c, err := openSQLite(context.Background(), uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeQuietly(c) })
	return c
}

// Runs statements on a connection, failing the test on the first error
func mustExec(t *testing.T, c *sqliteConn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := c.c.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// Database of rows of a 4000 byte body in the given journal mode
func messagesDB(t *testing.T, path, journal string, rows int) {
	t.Helper()
	c := testConn(t, path, "")
	mustExec(t, c, "PRAGMA journal_mode="+journal, "CREATE TABLE messages (body TEXT)")
	tx, err := c.c.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for range rows {
		if _, err := tx.Exec("INSERT INTO messages VALUES (?)", strings.Repeat("x", 4000)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	closeQuietly(c)
}

// Single value of a query on a database file
func queryOne(t *testing.T, path, q string) string {
	t.Helper()
	c := testConn(t, path, "")
	defer closeQuietly(c)
	var v sql.NullString
	if err := c.c.QueryRowContext(context.Background(), q).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v.String
}

// Runs the pre-flight entry in process and returns its exit code, stdout and stderr
func runPreflight(ctx context.Context, h string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := PreflightStateDBMain(ctx, []string{h}, &out, &errOut)
	return code, out.String(), errOut.String()
}

// Path the pre-flight printed as its published snapshot
func printedPath(t *testing.T, stdout string) string {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout %q: %v", stdout, err)
	}
	return result["path"].(string)
}

// Sorted globbed names under a home
func globNames(t *testing.T, h, pattern string) []string {
	t.Helper()
	paths, err := pypath.Glob(h, pattern)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	sort.Strings(names)
	return names
}

func TestPreflightFinishesWhileAnotherConnectionKeepsWriting(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(h, 0o755)
	db := filepath.Join(h, "state.db")
	messagesDB(t, db, "WAL", 4000)
	writer := testConn(t, db, "_pragma=busy_timeout(5000)")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				writer.c.ExecContext(context.Background(), "INSERT INTO messages VALUES ('live turn')")
			}
		}
	})
	code, stdout, stderr := runPreflight(context.Background(), h)
	close(stop)
	wg.Wait()
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := queryOne(t, printedPath(t, stdout), "PRAGMA integrity_check"); got != "ok" {
		t.Fatalf("integrity %s", got)
	}
}

func TestPreflightCapturesCommittedWALWithoutApplicationState(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	os.MkdirAll(h, 0o755)
	db := filepath.Join(h, "state.db")
	writer := testConn(t, db, "")
	mustExec(t, writer, "PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "CREATE TABLE messages (body TEXT)",
		"PRAGMA wal_checkpoint(TRUNCATE)", "INSERT INTO messages VALUES ('committed only in WAL')")
	if info, err := os.Stat(db + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("wal %v", err)
	}
	control := filepath.Join(tmp, "raw.db")
	raw, _ := os.ReadFile(db)
	os.WriteFile(control, raw, 0o644)
	if got := queryOne(t, control, "SELECT count(*) FROM messages"); got != "0" {
		t.Fatalf("control rows %s", got)
	}
	for range 3 {
		code, stdout, stderr := runPreflight(context.Background(), h)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		snap := printedPath(t, stdout)
		if got := queryOne(t, snap, "SELECT group_concat(body) FROM messages"); got != "committed only in WAL" {
			t.Fatalf("rows %q", got)
		}
		if got := queryOne(t, snap, "PRAGMA integrity_check"); got != "ok" {
			t.Fatalf("integrity %s", got)
		}
	}
	previous := globNames(t, h, "state.db.pre-update-emergency-*.bak")
	if len(previous) != 2 {
		t.Fatalf("kept %v", previous)
	}
	closeQuietly(writer)
	os.WriteFile(db, []byte("broken database"), 0o644)
	if code, _, _ := runPreflight(context.Background(), h); code == 0 {
		t.Fatal("broken database published")
	}
	if got := globNames(t, h, "state.db.pre-update-emergency-*.bak"); strings.Join(got, ",") != strings.Join(previous, ",") {
		t.Fatalf("snapshots %v want %v", got, previous)
	}
	if got := globNames(t, h, "*.partial"); len(got) != 0 {
		t.Fatalf("staging %v", got)
	}
}

func TestRollbackJournalCopyLetsALiveWriterCommitBetweenSteps(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "state.db")
	messagesDB(t, db, "DELETE", 2000)
	writer := testConn(t, db, "_pragma=busy_timeout(0)")
	var unread []int
	var writeErrors []string
	real := connBackup
	connBackup = func(src, dst pysqlite.RawConn, pages int, progress func(int, int, int) error, sleep time.Duration) error {
		between := func(status, remaining, total int) error {
			if status == sqlite3.SQLITE_OK {
				if _, err := writer.c.ExecContext(context.Background(), "INSERT INTO messages VALUES ('live turn')"); err != nil {
					writeErrors = append(writeErrors, err.Error())
				} else {
					unread = append(unread, remaining)
				}
			}
			return progress(status, remaining, total)
		}
		return real(src, dst, pages, between, sleep)
	}
	t.Cleanup(func() { connBackup = real })
	snapshot := filepath.Join(tmp, "snapshot.db")
	copied, err := safeCopyDB(context.Background(), db, snapshot, 10.0)
	if err != nil {
		t.Fatal(err)
	}
	if len(writeErrors) != 0 {
		t.Fatalf("write errors %v", writeErrors)
	}
	gotIn := false
	for _, r := range unread {
		gotIn = gotIn || r > 0
	}
	if !gotIn {
		t.Fatalf("no commit before the copy finished %v", unread)
	}
	if copied {
		if got := queryOne(t, snapshot, "PRAGMA integrity_check"); got != "ok" {
			t.Fatalf("integrity %s", got)
		}
	} else if _, err := os.Stat(snapshot); err == nil {
		t.Fatal("refused copy left its destination")
	}
}

func TestANewPreflightReclaimsStagingAbandonedByAKilledOne(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(h, 0o755)
	messagesDB(t, filepath.Join(h, "state.db"), "WAL", 10)
	abandoned := filepath.Join(h, "state.db.pre-update-emergency-killed.partial")
	os.WriteFile(abandoned, bytes.Repeat([]byte("x"), 4096), 0o644)
	hoursAgo := time.Now().Add(-3 * time.Hour)
	os.Chtimes(abandoned, hoursAgo, hoursAgo)
	inFlight := filepath.Join(h, "state.db.pre-update-emergency-running.partial")
	os.WriteFile(inFlight, bytes.Repeat([]byte("x"), 4096), 0o644)
	if code, _, stderr := runPreflight(context.Background(), h); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := globNames(t, h, "state.db.pre-update-emergency-*.partial*"); len(got) != 1 || got[0] != filepath.Base(inFlight) {
		t.Fatalf("staging %v", got)
	}
}

func TestPreflightWithoutStateDBReportsAFreshInstall(t *testing.T) {
	code, stdout, _ := runPreflight(context.Background(), t.TempDir())
	if code != 0 || stdout != "{\"path\": null, \"message\": \"state.db not found (fresh install?)\"}\n" {
		t.Fatalf("exit %d stdout %q", code, stdout)
	}
	var errOut bytes.Buffer
	if code := PreflightStateDBMain(context.Background(), nil, &bytes.Buffer{}, &errOut); code != 1 || !strings.Contains(errOut.String(), "IndexError") {
		t.Fatalf("missing argument exit %d %q", code, errOut.String())
	}
}

func TestCopyHonestyTruncatedDB(t *testing.T) {
	tmp := t.TempDir()
	good := filepath.Join(tmp, "good.db")
	messagesDB(t, good, "DELETE", 50)
	raw, _ := os.ReadFile(good)
	trunc := filepath.Join(tmp, "trunc.db")
	os.WriteFile(trunc, raw[:len(raw)-4096-100], 0o644)
	if VerifySQLiteIntegrity(trunc, IntegrityOptions{}).Valid {
		t.Fatal("truncated database verified")
	}
	dst := filepath.Join(tmp, "copy.db")
	if copied, err := safeCopyDB(context.Background(), trunc, dst, 10.0); copied || err != nil {
		t.Fatalf("copied %v err %v", copied, err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("left a partial destination")
	}
}

func TestSafeCopyDBCopiesValidDatabase(t *testing.T) {
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "test.db"), filepath.Join(tmp, "copy.db")
	c := testConn(t, src, "")
	mustExec(t, c, "CREATE TABLE t (x INTEGER)", "INSERT INTO t VALUES (42)")
	closeQuietly(c)
	if copied, err := safeCopyDB(context.Background(), src, dst, 10.0); !copied || err != nil {
		t.Fatalf("copied %v err %v", copied, err)
	}
	if got := queryOne(t, dst, "SELECT x FROM t"); got != "42" {
		t.Fatalf("rows %s", got)
	}
	if info, _ := os.Stat(dst); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestSafeCopyDBAbortsWhenSourceRemainsBusyPastDeadline(t *testing.T) {
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "locked.db"), filepath.Join(tmp, "copy.db")
	os.WriteFile(src, nil, 0o644)
	os.WriteFile(dst, []byte("partial"), 0o644)
	base := time.Unix(100, 0)
	clock := []time.Time{base, base.Add(500 * time.Millisecond), base.Add(1100 * time.Millisecond)}
	realClock, realBackup, realRemove := monotonic, connBackup, removeFile
	t.Cleanup(func() { monotonic, connBackup, removeFile = realClock, realBackup, realRemove })
	monotonic = func() time.Time {
		now := clock[0]
		if len(clock) > 1 {
			clock = clock[1:]
		}
		return now
	}
	var source *sqliteConn
	var destination *sqliteConn
	sourceTimeout := ""
	connBackup = func(s, d pysqlite.RawConn, pages int, progress func(int, int, int) error, sleep time.Duration) error {
		if pages <= 0 || sleep <= 0 {
			t.Fatalf("pages %d sleep %v", pages, sleep)
		}
		source, destination = s.(*sqliteConn), d.(*sqliteConn)
		source.c.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&sourceTimeout)
		if err := progress(sqlite3.SQLITE_BUSY, 0, 1); err != nil {
			return err
		}
		return progress(sqlite3.SQLITE_BUSY, 0, 1)
	}
	removeFile = func(p string) error {
		if err := destination.c.PingContext(context.Background()); !errors.Is(err, sql.ErrConnDone) {
			t.Fatalf("destination still open before unlink: %v", err)
		}
		return realRemove(p)
	}
	if copied, err := safeCopyDB(context.Background(), src, dst, 1.0); copied || err != nil {
		t.Fatalf("copied %v err %v", copied, err)
	}
	if sourceTimeout != "0" {
		t.Fatalf("source busy timeout %q", sourceTimeout)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("destination kept")
	}
}
