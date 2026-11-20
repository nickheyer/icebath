package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const walHolderHelperEnv = "ICEBATH_TEST_WAL_HOLDER"

func TestWALResetVersionMatrix(t *testing.T) {
	cases := []struct {
		v    [3]int64
		want bool
	}{
		{[3]int64{3, 6, 23}, false}, {[3]int64{3, 7, 0}, true}, {[3]int64{3, 44, 5}, true}, {[3]int64{3, 44, 6}, false},
		{[3]int64{3, 44, 9}, false}, {[3]int64{3, 45, 0}, true}, {[3]int64{3, 46, 1}, true}, {[3]int64{3, 50, 4}, true},
		{[3]int64{3, 50, 6}, true}, {[3]int64{3, 50, 7}, false}, {[3]int64{3, 50, 99}, false}, {[3]int64{3, 51, 0}, true},
		{[3]int64{3, 51, 2}, true}, {[3]int64{3, 51, 3}, false}, {[3]int64{3, 52, 0}, false},
	}
	for _, c := range cases {
		info := NewVersionTuple(c.v[0], c.v[1], c.v[2])
		if got := WALResetVulnerable(&info); got != c.want {
			t.Errorf("WALResetVulnerable(%v) = %v want %v", c.v, got, c.want)
		}
	}
}

// Connection wrapper failing any statement the fault function rejects
type faultConn struct {
	SQLiteConn
	fault func(normalized string) error
}

func (c *faultConn) check(query string) error {
	return c.fault(strings.ReplaceAll(strings.ToLower(query), " ", ""))
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := c.check(query); err != nil {
		return nil, err
	}
	return c.SQLiteConn.ExecContext(ctx, query, args...)
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if err := c.check(query); err != nil {
		return nil, err
	}
	return c.SQLiteConn.QueryContext(ctx, query, args...)
}

// Seeds a WAL database holding one row of x
func seedWALRow(t *testing.T, path string, x int) {
	t.Helper()
	seedWALDB(t, path)
	conn := openWALTestConn(t, path)
	mustExecConn(t, conn, fmt.Sprintf("INSERT INTO t VALUES (%d)", x))
	conn.Close()
}

// Integer values of column x in table t
func tableValues(t *testing.T, conn SQLiteConn) map[int64]bool {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), "SELECT x FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var x int64
		if err := rows.Scan(&x); err != nil {
			t.Fatal(err)
		}
		out[x] = true
	}
	return out
}

func TestFreshDBUsesDeleteWhenVulnerable(t *testing.T) {
	upgradeFixture(t, "wal", true)
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "fresh.db"))
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "fresh.db", false); err != nil || mode != "delete" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if journalModeOf(t, conn) != "delete" {
		t.Fatal("fresh database not DELETE")
	}
}

func TestExistingWALLeftAloneWhenVulnerable(t *testing.T) {
	upgradeFixture(t, "wal", true)
	path := filepath.Join(t.TempDir(), "prior_wal.db")
	seedWALRow(t, path, 42)
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 30.0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "prior_wal.db", false); err != nil || mode != "wal" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if journalModeOf(t, conn) != "wal" || !tableValues(t, conn)[42] {
		t.Fatal("existing WAL database changed")
	}
}

func TestWALHolderHelperProcess(t *testing.T) {
	if os.Getenv(walHolderHelperEnv) != "1" {
		return
	}
	db, ready, done := os.Getenv("WAL_HOLDER_DB"), os.Getenv("WAL_HOLDER_READY"), os.Getenv("WAL_HOLDER_DONE")
	conn, err := ConnectSQLite(db, SQLiteConfig{Timeout: 30.0})
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	if _, err := conn.ExecContext(context.Background(), "INSERT INTO t VALUES (777)"); err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	if err := os.WriteFile(ready, []byte("ready"), 0o644); err != nil {
		os.Exit(2)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(done); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	conn.Close()
	os.Exit(0)
}

func TestSecondProcessOpenerKeepsWALWhenVulnerable(t *testing.T) {
	buf := upgradeFixture(t, "wal", true)
	dir := t.TempDir()
	db := filepath.Join(dir, "live_wal.db")
	seedWALRow(t, db, 1)
	ready, done := filepath.Join(dir, "holder.ready"), filepath.Join(dir, "holder.done")
	holder := exec.Command(os.Args[0], "-test.run=^TestWALHolderHelperProcess$")
	holder.Env = append(os.Environ(), walHolderHelperEnv+"=1", "WAL_HOLDER_DB="+db, "WAL_HOLDER_READY="+ready, "WAL_HOLDER_DONE="+done)
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- holder.Wait() }()
	defer func() {
		os.WriteFile(done, []byte("done"), 0o644)
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			holder.Process.Kill()
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatal("holder subprocess never became ready")
	}
	conn, err := ConnectSQLite(db, SQLiteConfig{Timeout: 30.0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	mode, err := ApplyWALWithFallback(context.Background(), conn, "live_wal.db", false)
	select {
	case err := <-exited:
		t.Fatalf("holder must still be alive here: %v", err)
	default:
	}
	if err != nil || mode != "wal" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if journalModeOf(t, conn) != "wal" {
		t.Fatal("live WAL downgraded")
	}
	if rows := tableValues(t, conn); len(rows) != 2 || !rows[1] || !rows[777] {
		t.Fatalf("rows = %v", rows)
	}
	if strings.Contains(buf.String(), "instead of enabling WAL") {
		t.Fatal("gate claimed it switched to DELETE")
	}
}

// Opens a holder that keeps an exclusive locking write transaction on the WAL database
func exclusiveHolder(t *testing.T, path string) SQLiteConn {
	t.Helper()
	holder := openWALTestConn(t, path)
	mustExecConn(t, holder, "PRAGMA locking_mode=EXCLUSIVE")
	mustExecConn(t, holder, "BEGIN IMMEDIATE")
	mustExecConn(t, holder, "INSERT INTO t VALUES (2)")
	return holder
}

func TestUnreadableModeKeepsJournalModeWhenVulnerable(t *testing.T) {
	buf := upgradeFixture(t, "wal", true)
	db := filepath.Join(t.TempDir(), "locked_wal.db")
	seedWALRow(t, db, 1)
	holder := exclusiveHolder(t, db)
	conn, err := ConnectSQLite(db, SQLiteConfig{Timeout: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchOne(context.Background(), conn, "PRAGMA journal_mode"); !isOperationalError(err) {
		t.Fatalf("probe under exclusive holder = %v", err)
	}
	mode, err := ApplyWALWithFallback(context.Background(), conn, "locked_wal.db", false)
	conn.Close()
	if err != nil || mode != "wal" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if !strings.Contains(buf.String(), "concurrent openers") || strings.Contains(buf.String(), "instead of enabling WAL") {
		t.Fatalf("logs = %s", buf.String())
	}
	mustExecConn(t, holder, "ROLLBACK")
	holder.Close()
	if journalModeOf(t, openWALTestConn(t, db)) != "wal" {
		t.Fatal("locked WAL database flipped")
	}
}

func TestExclusivelyOwnedFreshDBStillDowngrades(t *testing.T) {
	buf := upgradeFixture(t, "wal", true)
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "exclusive.db"))
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "exclusive.db", false); err != nil || mode != "delete" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if journalModeOf(t, conn) != "delete" || !strings.Contains(buf.String(), "instead of enabling WAL") {
		t.Fatalf("exclusive downgrade logs = %s", buf.String())
	}
}

func TestFlipLockConflictLeavesModeAlone(t *testing.T) {
	buf := upgradeFixture(t, "wal", true)
	conn := &faultConn{SQLiteConn: openWALTestConn(t, filepath.Join(t.TempDir(), "race.db")), fault: func(q string) error {
		if strings.Contains(q, "journal_mode=delete") {
			return operationalError("database is locked")
		}
		return nil
	}}
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "race.db", false); err != nil || mode != "delete" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if !strings.Contains(buf.String(), "concurrent openers") {
		t.Fatalf("logs = %s", buf.String())
	}
}

func TestConfiguredDeleteRefusesWhenProbeBlocked(t *testing.T) {
	resetWALOnceState(t)
	pinVar(t, &walResetVulnerable, func() bool { return false })
	pinVar(t, &resolveJournalMode, func(context.Context) string { return "delete" })
	db := filepath.Join(t.TempDir(), "cfg_delete.db")
	seedWALDB(t, db)
	holder := exclusiveHolder(t, db)
	conn, err := ConnectSQLite(db, SQLiteConfig{Timeout: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ApplyWALWithFallback(context.Background(), conn, "cfg_delete.db", false)
	conn.Close()
	if !isOperationalError(err) || !strings.Contains(err.Error(), "refusing to downgrade") {
		t.Fatalf("blocked configured delete = %v", err)
	}
	mustExecConn(t, holder, "ROLLBACK")
}

func TestProbeUnreadableTouchesNothing(t *testing.T) {
	resetWALOnceState(t)
	configureJournalMode(t, "wal")
	pinVar(t, &walResetVulnerable, func() bool { return false })
	buf := captureSlog(t)
	conn := &faultConn{SQLiteConn: openWALTestConn(t, filepath.Join(t.TempDir(), "nfs.db")), fault: func(q string) error {
		if strings.Contains(q, "journal_mode=wal") {
			return operationalError("locking protocol")
		}
		if q == "pragmajournal_mode" {
			return operationalError("database is locked")
		}
		return nil
	}}
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "nfs.db", false); err != nil || mode != "wal" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if !strings.Contains(buf.String(), "could not verify the on-disk journal mode") {
		t.Fatalf("logs = %s", buf.String())
	}
}
