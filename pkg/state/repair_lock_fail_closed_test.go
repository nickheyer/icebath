package state

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Makes the lock path unopenable as a file by standing a directory where the code expects one
func makeUnopenable(t *testing.T, lockPath string) {
	t.Helper()
	os.Remove(lockPath)
	if err := os.MkdirAll(lockPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666); err == nil {
		f.Close()
		t.Fatal("fixture precondition: the lock path still opens as a file")
	}
}

func TestFtsAdmissionFailsClosedWhenLockFileIsUnopenable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	makeUnopenable(t, dbPath+".fts_rebuild.lock")
	admitted := true
	if err := FTSRebuildAdmission(dbPath, nil, func(a bool) error { admitted = a; return nil }); err != nil {
		t.Fatal(err)
	}
	if admitted {
		t.Fatal("an unopenable lock file admitted the rebuild")
	}
}

func TestFtsAdmissionStillAdmitsAPathlessDb(t *testing.T) {
	admitted := false
	if err := FTSRebuildAdmission("", nil, func(a bool) error { admitted = a; return nil }); err != nil {
		t.Fatal(err)
	}
	if !admitted {
		t.Fatal("a pathless store was refused the rebuild")
	}
}

func TestRebuildFtsDefersWhenLockFileIsUnopenable(t *testing.T) {
	ctx := context.Background()
	db := usageOpen(t, filepath.Join(t.TempDir(), "state.db"))
	if !db.ftsEnabled.Load() {
		t.Skip("FTS5 unavailable in this build")
	}
	readSession(t, db, NewSession{ID: "s1", Source: "test"})
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "hello world"})
	if n, err := db.RebuildFTS(ctx); err != nil || n < 1 {
		t.Fatalf("rebuild with an openable lock %d %v", n, err)
	}
	makeUnopenable(t, db.path+".fts_rebuild.lock")
	if n, err := db.RebuildFTS(ctx); err != nil || n != 0 {
		t.Fatalf("rebuild with an unopenable lock %d %v", n, err)
	}
}

func TestRepairLockFailsClosedWhenLockFileIsUnopenable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	makeUnopenable(t, dbPath+".repair.lock")
	holding := true
	if err := crossProcessRepairLock(dbPath, func(h bool) error { holding = h; return nil }); err != nil {
		t.Fatal(err)
	}
	if holding {
		t.Fatal("an unopenable lock file granted the repair authority")
	}
}

func TestRepairSkipsSurgeryWhenLockFileIsUnopenable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("writable_schema corruption harness")
	}
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	buildHealthyDB(t, dbPath)
	corruptDuplicateFTS(t, dbPath)
	if _, sick, err := dbOpensCleanly(ctx, dbPath); err != nil || !sick {
		t.Fatalf("fixture precondition: corrupt store reads clean %v", err)
	}
	before := fileBytes(t, dbPath)
	makeUnopenable(t, dbPath+".repair.lock")
	report := repairReport(t, dbPath, true)
	if report.Repaired || !strings.Contains(report.Error, "repair lock") || report.BackupPath != "" {
		t.Fatalf("report %+v", report)
	}
	if backups := globNames(t, dir, "state.db.malformed-backup-*"); len(backups) != 0 {
		t.Fatalf("backups taken %v", backups)
	}
	if !bytes.Equal(fileBytes(t, dbPath), before) {
		t.Fatal("the damaged image changed")
	}
}

func TestWalRestorationReusesExclusiveRepairConnection(t *testing.T) {
	ctx := context.Background()
	writeHomeConfig(t, "{}\n")
	dbPath := filepath.Join(t.TempDir(), "state.db")
	conn := openRaw(t, dbPath)
	defer conn.Close()
	execAll(t, conn, "CREATE TABLE marker (value TEXT)")
	saved := repairConnectFn
	repairConnectFn = func(context.Context, string, float64) (*TrackedConn, error) {
		t.Fatal("WAL restoration reopened state.db outside the repair guard")
		return nil, nil
	}
	t.Cleanup(func() { repairConnectFn = saved })
	if err := restoreJournalModeAfterRepair(ctx, dbPath, "", conn); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode = strings.ToLower(mode); mode != "wal" && mode != "delete" {
		t.Fatalf("journal mode %q", mode)
	}
}

func TestRepairNeverReopensAfterTheGuardReleases(t *testing.T) {
	writeHomeConfig(t, "{}\n")
	dbPath := filepath.Join(t.TempDir(), "state.db")
	rawExecAll(t, dbPath, "CREATE TABLE sessions (name TEXT)", "INSERT INTO sessions VALUES ('seed')")
	forceUnhealthyProbe(t)
	savedSpace := repairScratchSpaceFn
	repairScratchSpaceFn = func(string) string { return "" }
	t.Cleanup(func() { repairScratchSpaceFn = savedSpace })
	swapStrategies(t, func(_ context.Context, _ string, report *RepairReport) error {
		report.Repaired, report.Strategy = true, "test_strategy"
		return nil
	})
	var events []string
	realGuard, realConnect := repairGuardFn, repairConnectFn
	repairGuardFn = func(ctx context.Context, path string, fn func(SQLiteConn, error) error) error {
		events = append(events, "guard-enter")
		err := realGuard(ctx, path, fn)
		events = append(events, "guard-exit")
		return err
	}
	repairConnectFn = func(ctx context.Context, path string, timeout float64) (*TrackedConn, error) {
		events = append(events, "connect")
		return realConnect(ctx, path, timeout)
	}
	t.Cleanup(func() { repairGuardFn, repairConnectFn = realGuard, realConnect })
	report := repairReport(t, dbPath, false)
	if !report.Repaired {
		t.Fatalf("report %+v", report)
	}
	exit := -1
	for i, e := range events {
		if e == "guard-exit" {
			exit = i
		}
	}
	if exit < 0 {
		t.Fatalf("guard never released %v", events)
	}
	for _, e := range events[exit+1:] {
		if e == "connect" {
			t.Fatalf("connection opened after the guard released %v", events)
		}
	}
}

// Plain WAL database with one seeded message row
func makeDurabilityDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	rawExecAll(t, path, "PRAGMA journal_mode=WAL", "CREATE TABLE sessions (session_id TEXT PRIMARY KEY)",
		"CREATE TABLE messages (id INTEGER PRIMARY KEY, body TEXT)", "INSERT INTO messages (body) VALUES ('seed')")
	return path
}

func TestConnectRepairDurableSetsMacosBarriers(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS write barriers")
	}
	ctx := context.Background()
	conn, err := connectRepairDurable(ctx, makeDurabilityDB(t), 5.0)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var synchronous, fullfsync int64
	if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA checkpoint_fullfsync").Scan(&fullfsync); err != nil {
		t.Fatal(err)
	}
	if synchronous != 2 || fullfsync != 1 {
		t.Fatalf("synchronous %d checkpoint_fullfsync %d", synchronous, fullfsync)
	}
}

func TestConnectRepairDurableIsAutocommit(t *testing.T) {
	ctx := context.Background()
	conn, err := connectRepairDurable(ctx, makeDurabilityDB(t), 5.0)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if auto, err := sqliteAutocommit(conn); err != nil || !auto {
		t.Fatalf("autocommit %v %v", auto, err)
	}
	execAll(t, conn, "VACUUM")
}

func TestRepairStillWorksThroughDurableConnection(t *testing.T) {
	path := makeDurabilityDB(t)
	report := repairReport(t, path, false)
	if report == nil {
		t.Fatal("no report")
	}
	conn := openRaw(t, path)
	defer conn.Close()
	if n := rawCount(t, conn, "SELECT COUNT(*) FROM messages"); n != 1 {
		t.Fatalf("messages %d", n)
	}
}

func TestIntegrityDamageClassifierMapsTreeIdsThroughRootpage(t *testing.T) {
	master := []SQLiteMasterRow{{12, "table", "messages_fts_data"}, {5, "table", "sessions"}, {15, "table", "gateway_routing"}, {40, "table", "archive_fts_data"}}
	if IntegrityDamageIsStructural([]string{"Tree 12 page 9: btreeInitPage() returns error code 11", "row 3 missing from index messages_fts_trigram_idx"}, master) {
		t.Fatal("FTS only damage classified as structural")
	}
	for _, line := range []string{
		"Tree 5  page 421385: btreeInitPage() returns error code 11",
		"Tree 15 page 15 cell 0: 2nd reference to page 5453",
		"Tree 40 page 40: btreeInitPage() returns error code 11",
		"row 1 missing from index sqlite_autoindex_delivery_obligations_1",
		"Freelist: invalid page number 167772160",
	} {
		if !IntegrityDamageIsStructural([]string{line}, master) {
			t.Fatalf("%q not structural", line)
		}
	}
	if IntegrityDamageIsStructural([]string{"Tree 999 page 1: garbage", "*** in database main ***"}, master) {
		t.Fatal("unknown tree lines classified as structural")
	}
}
