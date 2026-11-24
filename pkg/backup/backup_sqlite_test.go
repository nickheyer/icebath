package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	sqliteutil "github.com/nickheyer/icebath/pkg/utils/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Opens a test connection to a database file with extra DSN pragmas, closed with the test
func testConn(t *testing.T, path, query string) *sqliteConn {
	t.Helper()
	uri, err := pathutil.FileURL(path)
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

func TestRollbackJournalCopyLetsALiveWriterCommitBetweenSteps(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "state.db")
	messagesDB(t, db, "DELETE", 2000)
	writer := testConn(t, db, "_pragma=busy_timeout(0)")
	var unread []int
	var writeErrors []string
	real := connBackup
	connBackup = func(ctx context.Context, src, dst sqliteutil.RawConn, pages int, progress func(int, int, int) error, sleep time.Duration) error {
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
		return real(ctx, src, dst, pages, between, sleep)
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
	connBackup = func(_ context.Context, s, d sqliteutil.RawConn, pages int, progress func(int, int, int) error, sleep time.Duration) error {
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
