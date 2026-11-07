package pysqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// Single connection to a database file, closed with the test
func openFileConn(t *testing.T, path string) *sql.Conn {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestBackupStepsReportProgressAndCopyEveryPage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := openFileConn(t, filepath.Join(dir, "src.db"))
	if _, err := src.ExecContext(ctx, "CREATE TABLE t (body TEXT)"); err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if _, err := src.ExecContext(ctx, "INSERT INTO t VALUES (?)", string(make([]byte, 2000))); err != nil {
			t.Fatal(err)
		}
	}
	dst := openFileConn(t, filepath.Join(dir, "dst.db"))
	var statuses, remaining []int
	err := Backup(src, dst, 5, func(status, rem, total int) error {
		statuses = append(statuses, status)
		remaining = append(remaining, rem)
		if total == 0 {
			t.Fatal("zero page count")
		}
		return nil
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) < 2 || statuses[len(statuses)-1] != sqlite3.SQLITE_DONE || remaining[len(remaining)-1] != 0 {
		t.Fatalf("statuses %v remaining %v", statuses, remaining)
	}
	var n int
	if err := dst.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil || n != 200 {
		t.Fatalf("rows %d %v", n, err)
	}
}

func TestBackupRefusesTargetInTransactionAndPropagatesProgressErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := openFileConn(t, filepath.Join(dir, "src.db"))
	if _, err := src.ExecContext(ctx, "CREATE TABLE t (x)"); err != nil {
		t.Fatal(err)
	}
	dst := openFileConn(t, filepath.Join(dir, "dst.db"))
	if _, err := dst.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	var se *Error
	if err := Backup(src, dst, -1, nil, 0); !errors.As(err, &se) || se.Msg != "target is in transaction" || se.PyTypeName() != "OperationalError" {
		t.Fatalf("in transaction %v", err)
	}
	if _, err := dst.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop")
	if err := Backup(src, dst, -1, func(int, int, int) error { return stop }, 0); err != stop {
		t.Fatalf("progress error %v", err)
	}
	if err := Backup(src, src, -1, nil, 0); err == nil || err.Error() != "target cannot be the same connection instance" {
		t.Fatalf("same connection %v", err)
	}
}
