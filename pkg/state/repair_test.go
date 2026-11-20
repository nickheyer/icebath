package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

func repairHome(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX permission bits as a non root user")
	}
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func execAll(t *testing.T, conn SQLiteConn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := conn.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func openRaw(t *testing.T, path string) SQLiteConn {
	t.Helper()
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func makeRepairDB(t *testing.T, path string) {
	conn := openRaw(t, path)
	execAll(t, conn, "CREATE TABLE t (x)", "INSERT INTO t VALUES (1)")
	conn.Close()
}

func makeWALRepairDB(t *testing.T, path string) {
	conn := openRaw(t, path)
	execAll(t, conn, "PRAGMA journal_mode=WAL", "CREATE TABLE t (x)", "PRAGMA wal_checkpoint(TRUNCATE)")
	holder, err := ConnectSQLite("file:"+path+"?mode=ro", SQLiteConfig{Timeout: 5, URI: true})
	if err != nil {
		t.Fatal(err)
	}
	execAll(t, holder, "SELECT 1")
	execAll(t, conn, "INSERT INTO t VALUES (42)")
	conn.Close()
	holder.Close()
	if st, err := os.Stat(path + "-wal"); err != nil || !st.Mode().IsRegular() {
		t.Fatal("fixture precondition: -wal sidecar must survive with pending frames")
	}
}

func TestPreflightRepairsReadonlyDBInsideHome(t *testing.T) {
	h := repairHome(t)
	db := filepath.Join(h, "state.db")
	makeRepairDB(t, db)
	os.Chmod(db, 0o444)
	if err := PreflightDBWritability(context.Background(), db, "state.db", false); err != nil {
		t.Fatal(err)
	}
	if !pyfs.Access(db, 2) {
		t.Fatal("db still read only")
	}
}

func TestPreflightRepairsReadonlySidecars(t *testing.T) {
	h := repairHome(t)
	db := filepath.Join(h, "state.db")
	makeWALRepairDB(t, db)
	os.Chmod(db, 0o444)
	os.Chmod(db+"-wal", 0o444)
	if err := PreflightDBWritability(context.Background(), db, "state.db", false); err != nil {
		t.Fatal(err)
	}
	if !pyfs.Access(db, 2) || !pyfs.Access(db+"-wal", 2) {
		t.Fatal("db or wal still read only")
	}
}

func TestPreflightRepairsReadonlyParentDirectory(t *testing.T) {
	h := repairHome(t)
	sub := filepath.Join(h, "kanban")
	os.Mkdir(sub, 0o755)
	db := filepath.Join(sub, "kanban.db")
	makeRepairDB(t, db)
	os.Chmod(sub, 0o555)
	defer os.Chmod(sub, 0o755)
	if err := PreflightDBWritability(context.Background(), db, "kanban.db", false); err != nil {
		t.Fatal(err)
	}
	if !pyfs.Access(sub, 2) {
		t.Fatal("directory still read only")
	}
}

func TestPreflightRefusesOutsideHomeWithActionableError(t *testing.T) {
	repairHome(t)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	os.Mkdir(outside, 0o755)
	db := filepath.Join(outside, "custom.db")
	makeRepairDB(t, db)
	os.Chmod(db, 0o444)
	defer os.Chmod(db, 0o644)
	err := PreflightDBWritability(context.Background(), db, "custom.db", false)
	if !isOperationalError(err) || !strings.Contains(err.Error(), db) {
		t.Fatalf("want OperationalError naming %s, got %v", db, err)
	}
	if pyfs.Access(db, 2) {
		t.Fatal("file outside the home tree was chmodded")
	}
	makeWALRepairDB(t, filepath.Join(outside, "wal.db"))
	wal := filepath.Join(outside, "wal.db-wal")
	os.Chmod(wal, 0o444)
	defer os.Chmod(wal, 0o644)
	err = PreflightDBWritability(context.Background(), filepath.Join(outside, "wal.db"), "custom.db", false)
	if !isOperationalError(err) || !strings.Contains(err.Error(), wal) || !strings.Contains(err.Error(), "Do NOT delete the -wal") {
		t.Fatalf("wal refusal must name the wal and warn against deletion, got %v", err)
	}
}

func TestPreflightSidecarsUnlinkedBySiblingCloseAreNotReadonly(t *testing.T) {
	for _, recreates := range []bool{false, true} {
		repairHome(t)
		outside := filepath.Join(t.TempDir(), "elsewhere")
		os.Mkdir(outside, 0o755)
		db := filepath.Join(outside, "state.db")
		sibling := openRaw(t, db)
		execAll(t, sibling, "PRAGMA journal_mode=WAL", "CREATE TABLE t (x)")
		wal := db + "-wal"
		if _, err := os.Stat(wal); err != nil {
			t.Fatal("fixture precondition: the live sibling holds a -wal")
		}
		openSibling := []SQLiteConn{sibling}
		var nextOpener []SQLiteConn
		osAccess = func(path string, mode uint32) bool {
			if path != wal || len(openSibling) == 0 {
				return pyfs.Access(path, mode)
			}
			openSibling[0].Close()
			openSibling = nil
			verdict := pyfs.Access(path, mode)
			if recreates {
				conn := openRaw(t, db)
				nextOpener = append(nextOpener, conn)
				execAll(t, conn, "INSERT INTO t VALUES (1)")
			}
			return verdict
		}
		err := PreflightDBWritability(context.Background(), db, "state.db", false)
		osAccess = pyfs.Access
		for _, c := range nextOpener {
			c.Close()
		}
		if err != nil {
			t.Fatalf("recreates=%v: %v", recreates, err)
		}
		if len(openSibling) != 0 || len(nextOpener) != map[bool]int{false: 0, true: 1}[recreates] {
			t.Fatalf("recreates=%v: the race hook did not run", recreates)
		}
	}
}

func TestPreflightLeavesHealthyDBUntouched(t *testing.T) {
	h := repairHome(t)
	db := filepath.Join(h, "state.db")
	makeRepairDB(t, db)
	before, _ := os.Stat(db)
	if err := PreflightDBWritability(context.Background(), db, "state.db", false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(db)
	if before.Mode() != after.Mode() {
		t.Fatalf("mode changed %v -> %v", before.Mode(), after.Mode())
	}
	if err := PreflightDBWritability(context.Background(), ":memory:", "state.db", false); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrityDamageIsStructural(t *testing.T) {
	master := []SQLiteMasterRow{{2, "table", "sessions"}, {5, "table", "messages_fts_data"}, {7, "index", "idx_x"}}
	cases := []struct {
		lines []string
		want  bool
	}{
		{[]string{"*** in database main ***", "Tree 5 page 9 cell 0: invalid page number"}, false},
		{[]string{"Tree 2 page 3: btreeInitPage() returns error code 11"}, true},
		{[]string{"row 4 missing from index messages_fts_idx"}, false},
		{[]string{"row 4 missing from index idx_sessions_source"}, true},
		{[]string{"Freelist: size is 3 but should be 4"}, true},
		{[]string{"Tree 99 page 1: unmapped"}, false},
		{[]string{"ok"}, false},
	}
	for _, c := range cases {
		if got := IntegrityDamageIsStructural(c.lines, master); got != c.want {
			t.Errorf("%q: got %v want %v", c.lines, got, c.want)
		}
	}
	if !repairFTSObjectRE.MatchString("messages_fts_cjk_docsize") || repairFTSObjectRE.MatchString("archive_fts_data") {
		t.Fatal("fts object fullmatch drifted")
	}
}

func TestRepairFailureConsumesAttempt(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&SQLiteError{Class: SQLiteDatabaseError, Msg: "x", Code: 11, HasCode: true}, true},
		{&SQLiteError{Class: SQLiteDatabaseError, Msg: "x", Code: 267, HasCode: true}, true},
		{&SQLiteError{Class: SQLiteDatabaseError, Msg: "x", Code: 26, HasCode: true}, true},
		{&SQLiteError{Class: SQLiteOperationalError, Msg: "database is locked", Code: 5, HasCode: true}, false},
		{&SQLiteError{Class: SQLiteOperationalError, Msg: "database disk image is malformed"}, true},
		{&SQLiteError{Class: SQLiteOperationalError, Msg: "database or disk is full"}, false},
		{errors.New("database disk image is malformed"), false},
	}
	for _, c := range cases {
		if got := repairFailureConsumesAttempt(c.err); got != c.want {
			t.Errorf("%v: got %v want %v", c.err, got, c.want)
		}
	}
}

func TestRepairLedgerCapsAttemptsPerFingerprint(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	makeRepairDB(t, db)
	for i := 1; i <= maxPersistentRepairAttempts; i++ {
		if exhausted, err := persistentRepairAttemptsExhausted(db); err != nil || exhausted {
			t.Fatalf("attempt %d: exhausted=%v err=%v", i, exhausted, err)
		}
		recordRepairOutcome(db, false)
	}
	exhausted, err := persistentRepairAttemptsExhausted(db)
	if err != nil || !exhausted {
		t.Fatalf("budget must be exhausted after %d failures: %v %v", maxPersistentRepairAttempts, exhausted, err)
	}
	report, err := RepairStateDBSchema(context.Background(), db, true)
	if err != nil || !strings.Contains(report.Error, "automatic repair has already failed 3 times") {
		t.Fatalf("exhausted repair must refuse, got %+v %v", report, err)
	}
	conn := openRaw(t, db)
	execAll(t, conn, "PRAGMA journal_mode=DELETE", "CREATE TABLE grow (y)", "INSERT INTO grow VALUES (randomblob(70000))")
	conn.Close()
	if exhausted, _ := persistentRepairAttemptsExhausted(db); exhausted {
		t.Fatal("a changed file must reset the budget")
	}
	recordRepairOutcome(db, true)
	if _, err := os.Stat(repairLedgerPath(db)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a successful repair must clear the ledger")
	}
	head := make([]byte, 100)
	head[24], head[93], head[50] = 1, 1, 1
	masked := maskVolatileHeader(head)
	if masked[24] != 0 || masked[93] != 0 || masked[50] != 1 || head[24] != 1 {
		t.Fatal("volatile header mask drifted")
	}
}

func TestBackupDedupesAndPrunes(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	makeRepairDB(t, db)
	first, reason := backupDBFile(db)
	if first == "" {
		t.Fatal(reason)
	}
	again, _ := backupDBFile(db)
	if again != first {
		t.Fatalf("identical bytes must reuse %s, got %s", first, again)
	}
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(dir, "state.db.malformed-backup-20000101_00000"+string(rune('0'+i))), []byte("x"), 0o644)
	}
	pruneMalformedBackups(db, maxMalformedBackups)
	if got := existingMalformedBackups(db); len(got) != maxMalformedBackups || got[0] != first {
		t.Fatalf("prune kept %v", got)
	}
	live := openRaw(t, db)
	tracked, err := ConnectTracked(db, WithTrackingPath(db))
	if err != nil {
		t.Fatal(err)
	}
	if path, reason := backupDBFile(db); path != "" || !strings.Contains(reason, "still open in this process") {
		t.Fatalf("live connection must refuse the raw copy, got %q %q", path, reason)
	}
	tracked.Close()
	live.Close()
}

func makeMessagesDB(t *testing.T, path string) SQLiteConn {
	conn := openRaw(t, path)
	execAll(t, conn, "CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT, started_at REAL)",
		"CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, content TEXT, timestamp REAL)",
		"CREATE INDEX idx_messages_session ON messages(session_id)",
		"INSERT INTO sessions VALUES ('s1', 'cli', 1.0)",
		"INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1', 'user', 'hello', 1.0)")
	return conn
}

func TestRepairDedupesDuplicateSchemaOnSnapshot(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := filepath.Join(t.TempDir(), "state.db")
	conn := makeMessagesDB(t, db)
	execAll(t, conn, "PRAGMA writable_schema=ON",
		"INSERT INTO sqlite_master SELECT type, name, tbl_name, rootpage, sql FROM sqlite_master WHERE name = 'idx_messages_session'",
		"PRAGMA writable_schema=OFF")
	conn.Close()
	if _, sick, err := dbOpensCleanly(context.Background(), db); err != nil || !sick {
		t.Fatalf("fixture must be malformed: sick=%v err=%v", sick, err)
	}
	report, err := RepairStateDBSchema(context.Background(), db, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Repaired || report.Strategy != "dedup_schema" || report.BackupPath == "" || !report.HasJournalModeBefore {
		t.Fatalf("report %+v", report)
	}
	if _, sick, err := dbOpensCleanly(context.Background(), db); err != nil || sick {
		t.Fatalf("repaired db must open cleanly: sick=%v err=%v", sick, err)
	}
	check := openRaw(t, db)
	row, err := fetchOne(context.Background(), check, "SELECT content FROM messages")
	check.Close()
	if err != nil || pyStr(row[0]) != "hello" {
		t.Fatalf("canonical rows must survive, got %v %v", row, err)
	}
	if _, err := os.Stat(db + ".repair-scratch"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("scratch snapshot left beside the db")
	}
	if _, err := os.Stat(repairLedgerPath(db)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful repair must leave no ledger")
	}
	healthy, err := RepairStateDBSchema(context.Background(), db, true)
	if err != nil || !healthy.Repaired || healthy.Strategy != "already_healthy" {
		t.Fatalf("healthy re-run %+v %v", healthy, err)
	}
	missing, _ := RepairStateDBSchema(context.Background(), db+".absent", true)
	if missing.Error != db+".absent does not exist" {
		t.Fatalf("missing db report %+v", missing)
	}
}

func TestStructuralDamageProbeAndClaim(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	conn := makeMessagesDB(t, db)
	conn.Close()
	if damaged, err := StateDBHasStructuralDamage(db); err != nil || damaged {
		t.Fatalf("healthy db: damaged=%v err=%v", damaged, err)
	}
	if !claimRepairAttempt(db) || claimRepairAttempt(db) {
		t.Fatal("repair attempt claim must succeed exactly once per path")
	}
	handle := tryAcquireAutoMaintenanceLock(db)
	if handle == nil {
		t.Fatal("first auto maintenance lock must succeed")
	}
	releaseAutoMaintenanceLock(handle)
	if again := tryAcquireAutoMaintenanceLock(db); again == nil {
		t.Fatal("released lock must be reacquirable")
	} else {
		releaseAutoMaintenanceLock(again)
	}
}
