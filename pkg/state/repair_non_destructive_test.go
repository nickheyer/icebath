package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Session names of a plain repair fixture database
func fixtureNames(t *testing.T, path, query string) map[string]bool {
	t.Helper()
	conn := openRaw(t, path)
	defer conn.Close()
	rows, err := conn.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		out[value] = true
	}
	return out
}

// Failed attempt count of the repair ledger, zero when the ledger is absent
func ledgerFailures(t *testing.T, path string) int64 {
	t.Helper()
	n, err := ledgerFailedAttempts(readRepairLedger(path))
	if err != nil {
		t.Fatal(err)
	}
	return n.Int64()
}

// Strategy runner marking the scratch repaired after inserting a session name into it
func insertingStrategy(t *testing.T, name, strategy string) func(context.Context, string, *RepairReport) error {
	return func(_ context.Context, scratch string, report *RepairReport) error {
		rawExecAll(t, scratch, "INSERT INTO sessions (name) VALUES ('"+name+"')")
		report.Repaired, report.Strategy = true, strategy
		return nil
	}
}

// Strategy runner reporting failure without touching the scratch
func failingStrategy(calls *int) func(context.Context, string, *RepairReport) error {
	return func(_ context.Context, _ string, report *RepairReport) error {
		if calls != nil {
			*calls++
		}
		report.Repaired, report.Strategy = false, ""
		return nil
	}
}

func TestStrategiesNeverReceiveTheLiveDatabase(t *testing.T) {
	corrupt := corruptDB(t)
	var seen []string
	real := repairStrategiesFn
	swapStrategies(t, func(ctx context.Context, path string, report *RepairReport) error {
		seen = append(seen, path)
		return real(ctx, path, report)
	})
	repairReport(t, corrupt, true)
	if len(seen) == 0 {
		t.Fatal("the repair path did not run at all")
	}
	for _, path := range seen {
		if path == corrupt || !strings.HasSuffix(path, ".repair-scratch") {
			t.Fatalf("strategy received %s", path)
		}
	}
}

func TestFailedRepairLeavesTheOriginalByteIdentical(t *testing.T) {
	corrupt := corruptDB(t)
	before := sha256Of(t, corrupt)
	report := repairReport(t, corrupt, true)
	if report.Repaired {
		t.Fatal("fixture precondition: this corruption is not automatically repairable")
	}
	if sha256Of(t, corrupt) != before {
		t.Fatal("a repair that failed rewrote the database anyway")
	}
}

func TestFailedRepairLeavesNoScratchFileBehind(t *testing.T) {
	corrupt := corruptDB(t)
	repairReport(t, corrupt, true)
	if leftovers := globNames(t, filepath.Dir(corrupt), "*repair-scratch*"); len(leftovers) != 0 {
		t.Fatalf("scratch left behind %v", leftovers)
	}
}

func TestFailedRepairStillTakesTheForensicBackup(t *testing.T) {
	corrupt := corruptDB(t)
	report := repairReport(t, corrupt, true)
	if report.BackupPath == "" {
		t.Fatal("the pre-repair forensic copy is still required")
	}
	if _, err := os.Stat(report.BackupPath); err != nil {
		t.Fatal(err)
	}
}

func TestSuccessfulRepairIsPromotedOverTheOriginal(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	writePopulatedDB(t, db)
	forceUnhealthyProbe(t)
	swapStrategies(t, insertingStrategy(t, "healed-on-scratch", "test_strategy"))
	if report := repairReport(t, db, true); !report.Repaired {
		t.Fatalf("report %+v", report)
	}
	if !fixtureNames(t, db, "SELECT name FROM sessions")["healed-on-scratch"] {
		t.Fatal("the repaired copy was never promoted over the original")
	}
	if leftovers := globNames(t, filepath.Dir(db), "*repair-scratch*"); len(leftovers) != 0 {
		t.Fatalf("scratch left behind %v", leftovers)
	}
}

// Waits up to the deadline for a path to exist
func waitForFile(t *testing.T, path string, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCommittedWriterAfterStagingIsNeverLost(t *testing.T) {
	for _, journalMode := range []string{"delete", "wal"} {
		t.Run(journalMode, func(t *testing.T) {
			dir := t.TempDir()
			db := filepath.Join(dir, "state.db")
			makeRepairTestDB(t, db, journalMode)
			forceUnhealthyProbe(t)
			ready, start, result := filepath.Join(dir, "ready"), filepath.Join(dir, "start"), filepath.Join(dir, "result")
			writer := spawnHelper(t, "writer-after-stage", db, ready, start, result)
			writer.Stderr = os.Stderr
			if err := writer.Start(); err != nil {
				t.Fatal(err)
			}
			waitForFile(t, ready, 20*time.Second, "writer process readiness")
			swapStrategies(t, func(_ context.Context, scratch string, report *RepairReport) error {
				rawExecAll(t, scratch, "INSERT INTO sessions (name) VALUES ('repaired-before-race')")
				if err := os.WriteFile(start, []byte("go"), 0o644); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Second)
				report.Repaired, report.Strategy = true, "race_test"
				return nil
			})
			report := repairReport(t, db, false)
			done := make(chan error, 1)
			go func() { done <- writer.Wait() }()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				writer.Process.Kill()
				t.Fatal("writer process did not finish")
			}
			raw, err := os.ReadFile(result)
			if err != nil {
				t.Fatal(err)
			}
			outcome, detail, _ := strings.Cut(string(raw), "\x00")
			if outcome != "failed" && outcome != "committed" {
				t.Fatalf("writer outcome %q %q", outcome, detail)
			}
			if !report.Repaired {
				t.Fatalf("report %+v", report)
			}
			names := fixtureNames(t, db, "SELECT name FROM sessions")
			bodies := fixtureNames(t, db, "SELECT body FROM messages")
			if !names["repaired-before-race"] {
				t.Fatal("the repaired marker was lost")
			}
			if outcome == "committed" && !bodies["committed-after-stage"] {
				t.Fatal("a writer reported a successful commit, but transactional repair promotion silently overwrote it")
			}
		})
	}
}

func TestEnvironmentalAbortsDoNotBurnRepairLedger(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	makeRepairTestDB(t, db, "delete")
	forceUnhealthyProbe(t)
	savedSpace := repairScratchSpaceFn
	repairScratchSpaceFn = func(string) string { return "temporary disk pressure" }
	t.Cleanup(func() { repairScratchSpaceFn = savedSpace })
	for range 3 {
		if report := repairReport(t, db, false); report.Repaired || report.Error != "temporary disk pressure" {
			t.Fatalf("report %+v", report)
		}
	}
	if _, err := os.Stat(repairLedgerPath(db)); err == nil {
		t.Fatal("environmental aborts must not consume attempts")
	}
	repairScratchSpaceFn = func(string) string { return "" }
	swapStrategies(t, insertingStrategy(t, "after-aborts", "after_environmental_aborts"))
	if report := repairReport(t, db, false); !report.Repaired || report.Strategy != "after_environmental_aborts" {
		t.Fatalf("report %+v", report)
	}
}

func TestActualStrategyFailureStillConsumesOneAttempt(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	makeRepairTestDB(t, db, "delete")
	forceUnhealthyProbe(t)
	swapStrategies(t, failingStrategy(nil))
	if report := repairReport(t, db, false); report.Repaired {
		t.Fatalf("report %+v", report)
	}
	if n := ledgerFailures(t, db); n != 1 {
		t.Fatalf("failed_attempts %d", n)
	}
}

func TestRepairOutcomeIsRecordedWhileCrossProcessLockIsHeld(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	makeRepairTestDB(t, db, "delete")
	forceUnhealthyProbe(t)
	swapStrategies(t, failingStrategy(nil))
	released := false
	var observed []string
	realLock, realRecord := repairLockFn, repairRecordFn
	repairLockFn = func(path string, fn func(bool) error) error {
		err := realLock(path, fn)
		released = true
		return err
	}
	repairRecordFn = func(path string, repaired bool) {
		if released {
			t.Error("repair outcome was recorded after the cross-process lock released")
		}
		observed = append(observed, runHelper(t, spawnHelper(t, "probe-repair-lock", db)))
		if repaired {
			t.Error("a failed repair was recorded as repaired")
		}
	}
	t.Cleanup(func() { repairLockFn, repairRecordFn = realLock, realRecord })
	if report := repairReport(t, db, false); report.Repaired {
		t.Fatalf("report %+v", report)
	}
	if len(observed) != 1 || observed[0] != "false" {
		t.Fatalf("lock probes %v", observed)
	}
}

func TestExhaustionIsRecheckedAfterAcquiringRepairLock(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	makeRepairTestDB(t, db, "delete")
	checks := 0
	savedExhausted, savedLive, savedLocked := repairExhaustedFn, repairLiveWriterFn, repairLockedFn
	repairExhaustedFn = func(string) (bool, error) {
		checks++
		return checks >= 2, nil
	}
	repairLiveWriterFn = func(context.Context, string) (bool, error) { return false, nil }
	surgeries := 0
	repairLockedFn = func(_ context.Context, _ string, _ bool, report *RepairReport, _ string) (*RepairReport, error) {
		surgeries++
		return report, nil
	}
	t.Cleanup(func() { repairExhaustedFn, repairLiveWriterFn, repairLockedFn = savedExhausted, savedLive, savedLocked })
	report := repairReport(t, db, false)
	if report.Repaired || !strings.Contains(report.Error, "automatic repair has already failed") || checks != 2 || surgeries != 0 {
		t.Fatalf("report %+v checks %d surgeries %d", report, checks, surgeries)
	}
}

func TestScratchBudgetCountsSidecarsInVacuumMultiplier(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(db, []byte(strings.Repeat("m", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db+"-wal", []byte(strings.Repeat("w", 900)), 0o644); err != nil {
		t.Fatal(err)
	}
	total := uint64(10_000_000_000)
	headroom := repairBackupHeadroomBytes(total)
	oldRequired := uint64(100+900+200) + uint64(headroom)
	fakeDiskUsage(t, total, oldRequired)
	msg := repairScratchSpaceError(db)
	if msg == "" || !strings.Contains(msg, "VACUUM may need another") {
		t.Fatalf("message %q", msg)
	}
}

// Snapshot copier failing the listed call numbers with the given error
func failingSnapshotCalls(t *testing.T, failures map[int]error) {
	t.Helper()
	real := repairSnapshotFn
	calls := 0
	repairSnapshotFn = func(ctx context.Context, src, dst string, srcConn, dstConn SQLiteConn) error {
		calls++
		if err, ok := failures[calls]; ok {
			return err
		}
		return real(ctx, src, dst, srcConn, dstConn)
	}
	t.Cleanup(func() { repairSnapshotFn = real })
}

func TestEnvironmentalPromotionFailuresDoNotBurnLedger(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	makeRepairTestDB(t, db, "delete")
	forceUnhealthyProbe(t)
	swapStrategies(t, func(_ context.Context, _ string, report *RepairReport) error {
		report.Repaired, report.Strategy = true, "promotion_environment_test"
		return nil
	})
	full := &SQLiteError{Class: SQLiteOperationalError, Msg: "database or disk is full"}
	real := repairSnapshotFn
	failingSnapshotCalls(t, map[int]error{2: full, 4: full, 6: full})
	for range 3 {
		if report := repairReport(t, db, false); report.Repaired || !strings.Contains(report.Error, "could not be promoted") {
			t.Fatalf("report %+v", report)
		}
	}
	if n := ledgerFailures(t, db); n != 0 {
		t.Fatalf("failed_attempts %d", n)
	}
	repairSnapshotFn = real
	if report := repairReport(t, db, false); !report.Repaired || report.Strategy != "promotion_environment_test" {
		t.Fatalf("report %+v", report)
	}
}

func TestCorruptPromotionFailureConsumesOneAttempt(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	makeRepairTestDB(t, db, "delete")
	forceUnhealthyProbe(t)
	swapStrategies(t, func(_ context.Context, _ string, report *RepairReport) error {
		report.Repaired, report.Strategy = true, "corrupt-promotion-test"
		return nil
	})
	failingSnapshotCalls(t, map[int]error{2: &SQLiteError{Class: SQLiteDatabaseError, Msg: "database disk image is malformed"}})
	if report := repairReport(t, db, false); report.Repaired {
		t.Fatalf("report %+v", report)
	}
	if n := ledgerFailures(t, db); n != 1 {
		t.Fatalf("failed_attempts %d", n)
	}
}

func TestSnapshotIncludesCommittedWalFrames(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	conn := openRaw(t, db)
	defer conn.Close()
	execAll(t, conn, "CREATE TABLE messages (body TEXT)")
	var mode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode %q %v", mode, err)
	}
	execAll(t, conn, "PRAGMA wal_autocheckpoint=0", "INSERT INTO messages VALUES ('committed-only-in-wal')")
	if _, err := os.Stat(db + "-wal"); err != nil {
		t.Fatal("no WAL sidecar")
	}
	scratch := filepath.Join(dir, "state.db.repair-scratch")
	if err := copyDatabaseSnapshot(ctx, db, scratch, nil, nil); err != nil {
		t.Fatal(err)
	}
	if bodies := fixtureNames(t, scratch, "SELECT body FROM messages"); len(bodies) != 1 || !bodies["committed-only-in-wal"] {
		t.Fatalf("scratch rows %v", bodies)
	}
}

func TestFailedWalRepairPreservesCommittedRowsSemantically(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	makeRepairTestDB(t, db, "delete")
	runHelper(t, spawnHelper(t, "hot-wal-row", db))
	if _, err := os.Stat(db + "-wal"); err != nil {
		t.Skip("SQLite/filesystem did not retain a hot WAL sidecar")
	}
	forceUnhealthyProbe(t)
	calls := 0
	swapStrategies(t, failingStrategy(&calls))
	if report := repairReport(t, db, false); report.Repaired || calls != 1 {
		t.Fatalf("report %+v calls %d", report, calls)
	}
	bodies := fixtureNames(t, db, "SELECT body FROM messages")
	if !bodies["seed"] || !bodies["committed-wal-row"] {
		t.Fatalf("rows %v", bodies)
	}
}

func TestSnapshotDeadlineHasAFloorAndScalesWithSourceSize(t *testing.T) {
	saved := repairSnapshotMinThroughputBytesPerSecond
	repairSnapshotMinThroughputBytesPerSecond = 256 * 1024
	t.Cleanup(func() { repairSnapshotMinThroughputBytesPerSecond = saved })
	db := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(db, make([]byte, repairPageSize), 0o644); err != nil {
		t.Fatal(err)
	}
	small, err := repairSnapshotTimeoutSeconds(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(db, 64*1024*1024); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db+"-wal", []byte(strings.Repeat("w", 8*1024*1024)), 0o644); err != nil {
		t.Fatal(err)
	}
	large, err := repairSnapshotTimeoutSeconds(db)
	if err != nil {
		t.Fatal(err)
	}
	if small < repairLockTimeoutSeconds || large <= small {
		t.Fatalf("deadlines small %v large %v", small, large)
	}
}

func TestTransactionalPromotionPreservesALiveWalReader(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	live, scratch := filepath.Join(dir, "state.db"), filepath.Join(dir, "scratch.db")
	for path, value := range map[string]string{live: "old", scratch: "repaired"} {
		conn := openRaw(t, path)
		execAll(t, conn, "CREATE TABLE messages (body TEXT)", "INSERT INTO messages VALUES ('"+value+"')")
		if path == live {
			var mode string
			if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
				t.Fatalf("journal mode %q %v", mode, err)
			}
		}
		conn.Close()
	}
	reader := openRaw(t, live)
	defer reader.Close()
	execAll(t, reader, "BEGIN")
	bodies := func() []any {
		one, err := fetchOne(ctx, reader, "SELECT body FROM messages")
		if err != nil {
			t.Fatal(err)
		}
		return one
	}
	if got := bodies(); len(got) != 1 || got[0] != "old" {
		t.Fatalf("reader saw %v", got)
	}
	if err := copyDatabaseSnapshot(ctx, scratch, live, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := bodies(); len(got) != 1 || got[0] != "old" {
		t.Fatalf("reader saw %v after promotion", got)
	}
	if fresh := fixtureNames(t, live, "SELECT body FROM messages"); len(fresh) != 1 || !fresh["repaired"] {
		t.Fatalf("fresh connection saw %v", fresh)
	}
	execAll(t, reader, "ROLLBACK")
}

func TestInterruptedSnapshotRollsBackDestination(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "source.db"), filepath.Join(dir, "destination.db")
	conn := openRaw(t, source)
	execAll(t, conn, "CREATE TABLE payloads (body BLOB)")
	for range 400 {
		if _, err := conn.ExecContext(ctx, "INSERT INTO payloads VALUES (?)", make([]byte, repairPageSize)); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	rawExecAll(t, destination, "CREATE TABLE marker (value TEXT)", "INSERT INTO marker VALUES ('original')")
	base := time.Now()
	ticks := 0
	saved := snapshotClock
	snapshotClock = func() time.Time {
		ticks++
		if ticks == 1 {
			return base
		}
		return base.Add(secondsOf(repairLockTimeoutSeconds + 1.0))
	}
	t.Cleanup(func() { snapshotClock = saved })
	err := copyDatabaseSnapshot(ctx, source, destination, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("copy err %v", err)
	}
	if marker := fixtureNames(t, destination, "SELECT value FROM marker"); len(marker) != 1 || !marker["original"] {
		t.Fatalf("destination rows %v", marker)
	}
}

func TestFailedPromotionReturnsFailureAndPreservesOriginal(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	writePopulatedDB(t, db)
	before := sha256Of(t, db)
	busy := &SQLiteError{Class: SQLiteOperationalError, Msg: "destination is busy"}
	real := repairSnapshotFn
	calls := 0
	repairSnapshotFn = func(ctx context.Context, src, dst string, srcConn, dstConn SQLiteConn) error {
		calls++
		if calls == 1 {
			return real(ctx, src, dst, srcConn, dstConn)
		}
		return busy
	}
	t.Cleanup(func() { repairSnapshotFn = real })
	forceUnhealthyProbe(t)
	swapStrategies(t, func(_ context.Context, _ string, report *RepairReport) error {
		report.Repaired, report.Strategy = true, "test_strategy"
		return nil
	})
	report := repairReport(t, db, false)
	if report.Repaired || report.Strategy != "" || !strings.Contains(report.Error, "could not be promoted") {
		t.Fatalf("report %+v", report)
	}
	if sha256Of(t, db) != before {
		t.Fatal("the original changed")
	}
	if leftovers := globNames(t, filepath.Dir(db), "*repair-scratch*"); len(leftovers) != 0 {
		t.Fatalf("scratch left behind %v", leftovers)
	}
}

func TestScratchSpaceGuardAccountsForSnapshotAndVacuum(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(db, []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	total := uint64(10_000_000_000)
	headroom := repairBackupHeadroomBytes(total)
	fakeDiskUsage(t, total, uint64(3*4096)+uint64(headroom)-1)
	msg := repairScratchSpaceError(db)
	if msg == "" || !strings.Contains(msg, "VACUUM may need") {
		t.Fatalf("message %q", msg)
	}
}

func TestStaleScratchIsRemovedBeforeHealthCheck(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	writePopulatedDB(t, db)
	scratch := db + ".repair-scratch"
	if err := os.WriteFile(scratch, []byte(strings.Repeat("crash debris", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	var checks []string
	saved := repairProbeFn
	repairProbeFn = func(context.Context, string) (string, bool, error) {
		checks = append(checks, "health")
		if _, err := os.Stat(scratch); err == nil {
			t.Error("stale scratch survived until the health check")
		}
		return "", false, nil
	}
	t.Cleanup(func() { repairProbeFn = saved })
	report := repairReport(t, db, false)
	if !report.Repaired || report.Strategy != "already_healthy" || len(checks) != 1 {
		t.Fatalf("report %+v checks %v", report, checks)
	}
	if _, err := os.Stat(scratch); err == nil {
		t.Fatal("scratch survived")
	}
}

func TestStaleScratchIsRemovedBeforeSpaceCheck(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	writePopulatedDB(t, db)
	scratch := db + ".repair-scratch"
	if err := os.WriteFile(scratch, []byte(strings.Repeat("crash debris", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	forceUnhealthyProbe(t)
	checked := false
	saved := repairScratchSpaceFn
	repairScratchSpaceFn = func(string) string {
		checked = true
		if _, err := os.Stat(scratch); err == nil {
			t.Error("stale scratch survived until the space check")
		}
		return "forced low space"
	}
	t.Cleanup(func() { repairScratchSpaceFn = saved })
	report := repairReport(t, db, false)
	if report.Repaired || report.Error != "forced low space" || !checked {
		t.Fatalf("report %+v checked %v", report, checked)
	}
	if _, err := os.Stat(scratch); err == nil {
		t.Fatal("scratch survived")
	}
}

func TestStaleScratchCleanupFailureAbortsBeforeProbe(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	writePopulatedDB(t, db)
	probed := false
	savedProbe, savedUnlink := repairProbeFn, repairUnlinkFn
	repairProbeFn = func(context.Context, string) (string, bool, error) {
		probed = true
		return "forced-unhealthy", true, nil
	}
	repairUnlinkFn = func(string) string { return "scratch is locked" }
	t.Cleanup(func() { repairProbeFn, repairUnlinkFn = savedProbe, savedUnlink })
	result, err := repairStateDBSchemaLocked(context.Background(), db, false, &RepairReport{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Repaired || !strings.Contains(result.Error, "stale repair snapshot") || probed {
		t.Fatalf("result %+v probed %v", result, probed)
	}
}
