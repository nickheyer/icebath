package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Count of rows in table t of the database at path, read through a fresh connection
func countRowsT(t *testing.T, path string) int64 {
	t.Helper()
	check, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	row, err := fetchOne(context.Background(), check, "SELECT count(*) FROM t")
	if err != nil {
		t.Fatal(err)
	}
	return row[0].(int64)
}

func TestTransactionClosesTheConnectionEvenWhenTheBodyRaises(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "t.db")
	conn, err := OpenDB(ctx, db, "t.db", WithWAL(false))
	if err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, conn, "CREATE TABLE t (x)")
	midTransaction := errors.New("mid-transaction")
	err = Transaction(ctx, conn, false, func(c SQLiteConn) error {
		mustExecConn(t, c, "INSERT INTO t VALUES (1)")
		return midTransaction
	})
	if !errors.Is(err, midTransaction) {
		t.Fatalf("Transaction = %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT 1"); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("closed connection accepted a statement: %v", err)
	}
	if n := countRowsT(t, db); n != 0 {
		t.Fatalf("rolled back rows = %d", n)
	}
	conn, err = OpenDB(ctx, db, "t.db", WithWAL(false))
	if err != nil {
		t.Fatal(err)
	}
	if err := Transaction(ctx, conn, true, func(c SQLiteConn) error {
		mustExecConn(t, c, "INSERT INTO t VALUES (2)")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT 1"); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("success path left the connection open: %v", err)
	}
	if n := countRowsT(t, db); n != 1 {
		t.Fatalf("committed rows = %d", n)
	}
}

func TestTransactionRollsBackAndClosesOnPanicAndToleratesBodyCommit(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "t.db")
	conn, err := OpenDB(ctx, db, "t.db", WithWAL(false))
	if err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, conn, "CREATE TABLE t (x)")
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		Transaction(ctx, conn, false, func(c SQLiteConn) error {
			mustExecConn(t, c, "INSERT INTO t VALUES (1)")
			panic("boom")
		})
	}()
	if n := countRowsT(t, db); n != 0 {
		t.Fatalf("panic left rows = %d", n)
	}
	conn, err = OpenDB(ctx, db, "t.db", WithWAL(false))
	if err != nil {
		t.Fatal(err)
	}
	if err := Transaction(ctx, conn, false, func(c SQLiteConn) error {
		mustExecConn(t, c, "INSERT INTO t VALUES (3)")
		mustExecConn(t, c, "COMMIT")
		return nil
	}); err != nil {
		t.Fatalf("commit with no open transaction must be a no op: %v", err)
	}
	if n := countRowsT(t, db); n != 1 {
		t.Fatalf("rows = %d", n)
	}
}

type trackingOpener struct{ opened []SQLiteConn }

func (o *trackingOpener) connect(database string, cfg SQLiteConfig) (SQLiteConn, error) {
	conn, err := ConnectSQLite(database, cfg)
	if err == nil {
		o.opened = append(o.opened, conn)
	}
	return conn, err
}

func TestOpenDBClosesTheHalfOpenConnectionWhenInitializeRaises(t *testing.T) {
	opener := &trackingOpener{}
	pinVar(t, &openDBConnect, opener.connect)
	broken := func(context.Context, SQLiteConn) error { return operationalError("boom") }
	_, err := OpenDB(context.Background(), filepath.Join(t.TempDir(), "x.db"), "x.db", WithWAL(false), WithInitialize(broken))
	if !isOperationalError(err) || errText(err) != "boom" {
		t.Fatalf("OpenDB = %v", err)
	}
	if len(opener.opened) != 1 {
		t.Fatalf("opened %d connections", len(opener.opened))
	}
	if _, err := opener.opened[0].ExecContext(context.Background(), "SELECT 1"); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("half open connection still usable: %v", err)
	}
}

func TestOpenDBAppliesThePragmaStack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "deeper", "s.db")
	pinVar(t, &resolveJournalMode, func(context.Context) string { return "wal" })
	pinVar(t, &walResetVulnerable, func() bool { return false })
	conn, err := OpenDB(ctx, path, "s.db", WithBusyTimeoutMS(1234), WithForeignKeys(), WithSynchronousFull())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := pragmaInt(t, conn, "busy_timeout"); got != 1234 {
		t.Fatalf("busy_timeout = %d", got)
	}
	if got := pragmaInt(t, conn, "foreign_keys"); got != 1 {
		t.Fatalf("foreign_keys = %d", got)
	}
	if got := pragmaInt(t, conn, "synchronous"); got != 2 {
		t.Fatalf("synchronous = %d", got)
	}
	if got := journalModeOf(t, conn); got != "wal" {
		t.Fatalf("journal_mode = %q", got)
	}
	plain, err := OpenDB(ctx, filepath.Join(t.TempDir(), "p.db"), "p.db", WithWAL(false))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if got := journalModeOf(t, plain); got != "delete" {
		t.Fatalf("wal disabled journal_mode = %q", got)
	}
	if got := pragmaInt(t, plain, "busy_timeout"); got != 5000 {
		t.Fatalf("default busy_timeout = %d", got)
	}
}

func TestOpenDBRetriesOnlyTheTransientLockedJournalSetup(t *testing.T) {
	ctx := context.Background()
	pinVar(t, &resolveJournalMode, func(context.Context) string { return "wal" })
	pinVar(t, &walResetVulnerable, func() bool { return false })
	lockedSets, failure := 0, "database is locked"
	pinVar(t, &openDBConnect, func(database string, cfg SQLiteConfig) (SQLiteConn, error) {
		conn, err := ConnectSQLite(database, cfg)
		if err != nil {
			return nil, err
		}
		return &faultConn{SQLiteConn: conn, fault: func(q string) error {
			if q == "pragmajournal_mode=wal" && lockedSets > 0 {
				lockedSets--
				return operationalError(failure)
			}
			return nil
		}}, nil
	})
	var sleeps []time.Duration
	pinVar(t, &openDBSleep, func(d time.Duration) { sleeps = append(sleeps, d) })
	dir := t.TempDir()
	lockedSets = 2
	conn, err := OpenDB(ctx, filepath.Join(dir, "l.db"), "l.db", WithWALLockRetries(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 2 || sleeps[0] != 10*time.Millisecond || sleeps[1] != 20*time.Millisecond {
		t.Fatalf("sleeps = %v", sleeps)
	}
	if got := journalModeOf(t, conn); got != "wal" {
		t.Fatalf("journal_mode = %q", got)
	}
	conn.Close()
	sleeps, lockedSets = nil, 1
	if _, err := OpenDB(ctx, filepath.Join(dir, "once.db"), "once.db"); errText(err) != failure || len(sleeps) != 0 {
		t.Fatalf("single attempt = %v sleeps %v", err, sleeps)
	}
	sleeps, lockedSets, failure = nil, 1, "DATABASE IS LOCKED"
	upper, err := OpenDB(ctx, filepath.Join(dir, "upper.db"), "upper.db", WithWALLockRetries(2))
	if err != nil || len(sleeps) != 1 {
		t.Fatalf("case folded lock retry = %v sleeps %v", err, sleeps)
	}
	upper.Close()
}

func TestAddColumnIfMissingAndWriteTxn(t *testing.T) {
	ctx := context.Background()
	conn, err := OpenDB(ctx, filepath.Join(t.TempDir(), "a.db"), "a.db", WithWAL(false))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	mustExecConn(t, conn, "CREATE TABLE t (x)")
	if added, err := AddColumnIfMissing(ctx, conn, "t", "y", "y INTEGER DEFAULT 0"); !added || err != nil {
		t.Fatalf("first add = %v %v", added, err)
	}
	if added, err := AddColumnIfMissing(ctx, conn, "t", "y", "y INTEGER DEFAULT 0"); added || err != nil {
		t.Fatalf("racing add = %v %v", added, err)
	}
	if _, err := AddColumnIfMissing(ctx, conn, "missing", "y", "y INTEGER"); !isOperationalError(err) {
		t.Fatalf("missing table = %v", err)
	}
	failed := errors.New("body failed")
	if err := WriteTxn(ctx, conn, func() error {
		mustExecConn(t, conn, "INSERT INTO t (x) VALUES (1)")
		return failed
	}); !errors.Is(err, failed) {
		t.Fatalf("WriteTxn = %v", err)
	}
	if err := WriteTxn(ctx, conn, func() error {
		mustExecConn(t, conn, "ROLLBACK")
		return failed
	}); !errors.Is(err, failed) {
		t.Fatalf("auto rolled back WriteTxn = %v", err)
	}
	if err := WriteTxn(ctx, conn, func() error {
		mustExecConn(t, conn, "INSERT INTO t (x) VALUES (2)")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	row, err := fetchOne(ctx, conn, "SELECT count(*), max(x) FROM t")
	if err != nil || row[0].(int64) != 1 || row[1].(int64) != 2 {
		t.Fatalf("rows = %v %v", row, err)
	}
}

func TestTransactionJoinsAnAlreadyOpenTransaction(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "j.db")
	conn, err := OpenDB(ctx, db, "j.db", WithWAL(false))
	if err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, conn, "CREATE TABLE t (x)")
	mustExecConn(t, conn, "BEGIN")
	mustExecConn(t, conn, "INSERT INTO t VALUES (1)")
	if err := Transaction(ctx, conn, false, func(c SQLiteConn) error {
		mustExecConn(t, c, "INSERT INTO t VALUES (2)")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := countRowsT(t, db); n != 2 {
		t.Fatalf("joined transaction rows = %d", n)
	}
	conn, err = OpenDB(ctx, db, "j.db", WithWAL(false))
	if err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, conn, "BEGIN")
	err = Transaction(ctx, conn, true, func(SQLiteConn) error { return nil })
	if errText(err) != "cannot start a transaction within a transaction" {
		t.Fatalf("immediate inside an open transaction = %v", err)
	}
}

func TestDeferredTransactionBeginsRightBeforeTheFirstWrite(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "lazy.db")
	setup, err := OpenDB(ctx, db, "lazy.db", WithWAL(false))
	if err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, setup, "CREATE TABLE t (x)")
	mustExecConn(t, setup, "INSERT INTO t VALUES (0)")
	setup.Close()
	other, err := ConnectSQLite(db, SQLiteConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := ConnectSQLite(db, SQLiteConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	err = Transaction(ctx, conn, false, func(c SQLiteConn) error {
		var n int
		if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
			return err
		}
		if auto, err := sqliteAutocommit(c); err != nil || !auto {
			t.Fatalf("a read opened a transaction: autocommit=%v err=%v", auto, err)
		}
		if _, err := other.ExecContext(ctx, "INSERT INTO t VALUES (1)"); err != nil {
			t.Fatalf("a sibling write was blocked by a read before the first write: %v", err)
		}
		if _, err := c.ExecContext(ctx, "  /* lead */ -- line\n CREATE TABLE u (y)"); err != nil {
			return err
		}
		var tables int
		if err := other.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE name = 'u'").Scan(&tables); err != nil || tables != 1 {
			t.Fatalf("DDL before the first write was not committed at once: %d %v", tables, err)
		}
		if _, err := c.ExecContext(ctx, "\n\t-- note\n insert INTO t VALUES (2)"); err != nil {
			return err
		}
		if auto, err := sqliteAutocommit(c); err != nil || auto {
			t.Fatalf("the first write did not begin a transaction: autocommit=%v err=%v", auto, err)
		}
		if _, err := other.ExecContext(ctx, "INSERT INTO t VALUES (3)"); !IsSQLiteLockError(err) {
			t.Fatalf("the sibling write after the first write = %v, want database is locked", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRowsT(t, db); n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
}

func TestDMLDetectionFollowsCPythonLstripSQL(t *testing.T) {
	for query, want := range map[string]bool{
		"INSERT INTO t VALUES (1)":                           true,
		"  update t SET x = 1":                               true,
		"-- c\nDELETE FROM t":                                true,
		"/* a */ /* b */Replace INTO t(x)":                   true,
		"SELECT 1":                                           false,
		"WITH a AS (SELECT 1) INSERT INTO t SELECT * FROM a": false,
		"CREATE TABLE z (x)":                                 false,
		"-- only a comment":                                  false,
		"/* unterminated":                                    false,
		"- INSERT":                                           false,
		"/ INSERT":                                           false,
		"ſINSERT":                                            false,
		"inſert INTO t":                                      false,
		"BEGIN IMMEDIATE":                                    false,
		"\f\r\ninsert":                                       true,
		"\vINSERT INTO t(x) ":                                false,
	} {
		if got := isDMLStatement(query); got != want {
			t.Errorf("isDMLStatement(%q) = %v, want %v", query, got, want)
		}
	}
}
