package state

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Journal mode of a store read on a fresh raw connection
func modeOf(t *testing.T, path string) string {
	t.Helper()
	conn := openRaw(t, path)
	defer conn.Close()
	var mode string
	if err := conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(mode)
}

// Configures database.journal_mode in a fresh home with the WAL reset gate answering not vulnerable
func configureRepairJournalMode(t *testing.T, mode string) {
	t.Helper()
	writeHomeConfig(t, "database:\n  journal_mode: "+mode+"\n")
	saved := walResetVulnerable
	walResetVulnerable = func() bool { return false }
	t.Cleanup(func() { walResetVulnerable = saved })
}

// Integer answer of a store query through the open store
func storeCount(t *testing.T, db *SessionDB, query string) int64 {
	t.Helper()
	row, err := db.readOne(context.Background(), query)
	if err != nil || row == nil {
		t.Fatalf("%s: %v %v", query, row, err)
	}
	n, _ := exactInt(row.Vals[row.Keys[0]])
	return n
}

// Holds the repair lock of the store from a child process until stop is called
func holdRepairLockFromChild(t *testing.T, dbPath string) func() {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX flock test")
	}
	line, stop := startHelper(t, spawnHelper(t, "hold-repair-lock", dbPath+".repair.lock"))
	if line != "locked" {
		stop()
		t.Fatalf("lock holder printed %q", line)
	}
	return stop
}

// Shortens the repair lock wait for the test
func shortRepairLockTimeout(t *testing.T) {
	t.Helper()
	saved := repairLockTimeoutSeconds
	repairLockTimeoutSeconds = 0.5
	t.Cleanup(func() { repairLockTimeoutSeconds = saved })
}

func TestDuplicateFtsMakesEveryStatementFail(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	corruptDuplicateFTS(t, db)
	conn := openRaw(t, db)
	defer conn.Close()
	_, err := fetchOne(context.Background(), conn, "PRAGMA journal_mode")
	if err == nil || !isDatabaseError(err) || !IsMalformedDBError(err) {
		t.Fatalf("journal_mode err %v", err)
	}
}

func TestGenericMalformedOpenDoesNotAttemptSchemaSurgery(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	savedWAL, savedRepair := applyWALFn, openRepairFn
	applyWALFn = func(context.Context, SQLiteConn, string, bool) (string, error) {
		return "", &SQLiteError{Class: SQLiteDatabaseError, Msg: "database disk image is malformed"}
	}
	repairs := 0
	openRepairFn = func(ctx context.Context, path string, backup bool) (*RepairReport, error) {
		repairs++
		return &RepairReport{}, nil
	}
	t.Cleanup(func() { applyWALFn, openRepairFn = savedWAL, savedRepair })
	_, err := NewSessionDB(context.Background(), db, false)
	if err == nil || !isDatabaseError(err) || !strings.Contains(err.Error(), "disk image is malformed") {
		t.Fatalf("open err %v", err)
	}
	if repairs != 0 {
		t.Fatalf("schema surgery attempted %d times", repairs)
	}
}

func TestRepairedDbSearchWorks(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	corruptDuplicateFTS(t, db)
	repairReport(t, db, true)
	store := usageOpen(t, db)
	if hits := storeCount(t, store, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'pizza'"); hits != 5 {
		t.Fatalf("pizza hits %d", hits)
	}
	if n := storeCount(t, store, "SELECT COUNT(*) FROM messages_fts"); n != 10 {
		t.Fatalf("indexed rows %d", n)
	}
}

func TestAutoHealAttemptedOncePerProcess(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	corruptDuplicateFTS(t, db)
	repairAttemptMu.Lock()
	repairAttemptedPaths = map[string]struct{}{}
	repairAttemptMu.Unlock()
	saved := openRepairFn
	calls := 0
	openRepairFn = func(context.Context, string, bool) (*RepairReport, error) {
		calls++
		return &RepairReport{Error: "x"}, nil
	}
	t.Cleanup(func() { openRepairFn = saved })
	for range 2 {
		if _, err := NewSessionDB(context.Background(), db, false); err == nil || !isDatabaseError(err) {
			t.Fatalf("open err %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("repair attempted %d times", calls)
	}
}

func TestUnrepairableFileFailsSafely(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(db, append([]byte("SQLite format 3\x00"), bytes.Repeat([]byte("\x00\xde\xad\xbe\xef"), 200)...), 0o644); err != nil {
		t.Fatal(err)
	}
	report := repairReport(t, db, true)
	if report.Repaired || report.Error == "" || report.BackupPath == "" {
		t.Fatalf("report %+v", report)
	}
	if _, err := os.Stat(report.BackupPath); err != nil {
		t.Fatal(err)
	}
}

// Whether the probe of a store reports it unhealthy
func probeSick(t *testing.T, path string) bool {
	t.Helper()
	_, sick, err := dbOpensCleanly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return sick
}

func TestFtsReadCorruptionRepairedInPlace(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	corruptFTSShadowSegments(t, db)
	if !probeSick(t, db) {
		t.Fatal("corrupt shadow segments read clean")
	}
	if report := repairReport(t, db, true); !report.Repaired {
		t.Fatalf("report %+v", report)
	}
	if probeSick(t, db) {
		t.Fatal("store still unhealthy after the rebuild")
	}
	store := usageOpen(t, db)
	if hits := storeCount(t, store, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'pizza'"); hits < 5 {
		t.Fatalf("pizza hits %d", hits)
	}
}

func TestFtsWriteCorruptionDetectedByWriteProbe(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	if probeSick(t, db) {
		t.Fatal("healthy store probed sick")
	}
	corruptFTSIndexData(t, db)
	conn := openRaw(t, db)
	if rawCount(t, conn, "SELECT COUNT(*) FROM sessions") < 1 || rawCount(t, conn, "SELECT COUNT(*) FROM messages") != 10 {
		t.Fatal("base table reads changed")
	}
	conn.Close()
	if !probeSick(t, db) {
		t.Fatal("the write probe reported a false ok")
	}
}

func TestFtsWriteCorruptionRepairedInPlace(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	corruptFTSIndexData(t, db)
	report := repairReport(t, db, true)
	if !report.Repaired || (report.Strategy != "rebuild_fts" && report.Strategy != "dedup_schema" && report.Strategy != "drop_fts_rebuild") {
		t.Fatalf("report %+v", report)
	}
	if probeSick(t, db) {
		t.Fatal("store still unhealthy after the rebuild")
	}
	store := usageOpen(t, db)
	if n := storeCount(t, store, "SELECT COUNT(*) FROM messages"); n != 10 {
		t.Fatalf("messages %d", n)
	}
	row, err := store.readOne(ctx, "SELECT id FROM sessions LIMIT 1")
	if err != nil || row == nil {
		t.Fatalf("session row %v %v", row, err)
	}
	readAppend(t, store, pyStr(rowValue(row, "id")), map[string]any{"role": "user", "content": "post repair pizza message"})
	if n := storeCount(t, store, "SELECT COUNT(*) FROM messages"); n != 11 {
		t.Fatalf("messages after append %d", n)
	}
	if hits := storeCount(t, store, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'pizza'"); hits < 5 {
		t.Fatalf("pizza hits %d", hits)
	}
}

func TestRepairRebuildsStaleBtreeIndexes(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	corruptBtreeIndex(t, db, "idx_messages_session")
	reason, sick, err := dbOpensCleanly(ctx, db)
	if err != nil || !sick || !strings.Contains(reason, "wrong # of entries in index idx_messages_session") {
		t.Fatalf("probe %q %v %v", reason, sick, err)
	}
	if report := repairReport(t, db, true); !report.Repaired || report.Strategy != "reindex_btree" {
		t.Fatalf("report %+v", report)
	}
	if probeSick(t, db) {
		t.Fatal("store still unhealthy after REINDEX")
	}
	conn := openRaw(t, db)
	defer conn.Close()
	check, err := fetchOne(ctx, conn, "PRAGMA integrity_check")
	if err != nil || len(check) == 0 || check[0] != "ok" {
		t.Fatalf("integrity_check %v %v", check, err)
	}
	if n := rawCount(t, conn, "SELECT count(*) FROM messages INDEXED BY idx_messages_session WHERE session_id IS NOT NULL"); n != 10 {
		t.Fatalf("rows through the rebuilt index %d", n)
	}
}

func TestRepairStaleBtreeIndexPreservesRows(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	sid := buildHealthyDB(t, db)
	corruptBtreeIndex(t, db, "idx_messages_session")
	if report := repairReport(t, db, false); report.Strategy != "reindex_btree" {
		t.Fatalf("report %+v", report)
	}
	store := usageOpen(t, db)
	msgs, err := store.GetMessages(context.Background(), sid, GetMessagesOptions{})
	if err != nil || len(msgs) != 10 || rowValue(msgs[0], "content") != "hello world 0" {
		t.Fatalf("messages %d %v", len(msgs), err)
	}
}

func TestRepairSkipsSurgeryWhileAnotherProcessHoldsTheLock(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	buildHealthyDB(t, db)
	corruptDuplicateFTS(t, db)
	shortRepairLockTimeout(t)
	stop := holdRepairLockFromChild(t, db)
	report := repairReport(t, db, true)
	stop()
	if report.Repaired || !strings.Contains(report.Error, "repair lock") || report.BackupPath != "" {
		t.Fatalf("report %+v", report)
	}
	if backups := globNames(t, dir, "state.db.malformed-backup-*"); len(backups) != 0 {
		t.Fatalf("backups %v", backups)
	}
	if !probeSick(t, db) {
		t.Fatal("the losing process healed the store")
	}
}

func TestRepairReportsSuccessWhenTheHolderAlreadyHealedTheDb(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	shortRepairLockTimeout(t)
	stop := holdRepairLockFromChild(t, db)
	report := repairReport(t, db, true)
	stop()
	if !report.Repaired || report.Strategy != "repaired_by_other_process" {
		t.Fatalf("report %+v", report)
	}
}

// Closes this process's cached header probe descriptors so child processes see no holder here
func releaseHeaderProbeFDs() {
	headerProbeMu.Lock()
	defer headerProbeMu.Unlock()
	for _, probe := range headerProbeFDs {
		probe.file.Close()
	}
	clear(headerProbeFDs)
	for _, f := range retiredHeaderProbeFDs {
		f.Close()
	}
	retiredHeaderProbeFDs = nil
}

func TestTwoProcessesRepairingAtOncePerformSurgeryOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX flock test")
	}
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	buildHealthyDB(t, db)
	corruptDuplicateFTS(t, db)
	releaseHeaderProbeFDs()
	procs := []*exec.Cmd{spawnHelper(t, "repair", db), spawnHelper(t, "repair", db)}
	outputs := make([]chan []byte, len(procs))
	for i, cmd := range procs {
		outputs[i] = make(chan []byte, 1)
		go func(cmd *exec.Cmd, out chan []byte) {
			cmd.Stderr = os.Stderr
			raw, err := cmd.Output()
			if err != nil {
				raw = append(raw, []byte("\nERROR "+err.Error())...)
			}
			out <- raw
		}(cmd, outputs[i])
	}
	var reports []map[string]any
	for _, out := range outputs {
		raw := <-out
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		var report map[string]any
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
			t.Fatalf("child output %s", raw)
		}
		reports = append(reports, report)
	}
	strategies := []string{}
	for _, report := range reports {
		if report["repaired"] != true {
			t.Fatalf("reports %v", reports)
		}
		strategies = append(strategies, report["strategy"].(string))
	}
	joined := strings.Join(strategies, " ")
	if !strings.Contains(joined, "already_healthy") && !strings.Contains(joined, "repaired_by_other_process") {
		t.Fatalf("strategies %v", strategies)
	}
	if backups := globNames(t, dir, "state.db.malformed-backup-*"); len(backups) != 1 {
		t.Fatalf("backups %v", backups)
	}
	conn := openRaw(t, db)
	defer conn.Close()
	check, err := fetchOne(ctx, conn, "PRAGMA integrity_check")
	if err != nil || len(check) == 0 || check[0] != "ok" || rawCount(t, conn, "SELECT COUNT(*) FROM messages") != 10 {
		t.Fatalf("integrity %v %v", check, err)
	}
}

func TestSchemaSurgeryBumpsTheSchemaCookie(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	corruptDuplicateFTS(t, db)
	probe := openRaw(t, db)
	execAll(t, probe, "PRAGMA writable_schema=ON")
	before := rawCount(t, probe, "PRAGMA schema_version")
	probe.Close()
	if report := repairReport(t, db, true); !report.Repaired {
		t.Fatalf("report %+v", report)
	}
	probe = openRaw(t, db)
	defer probe.Close()
	if after := rawCount(t, probe, "PRAGMA schema_version"); after == before {
		t.Fatal("schema cookie unchanged after surgery")
	}
}

// Replaces the backup step with a refusal carrying the given reason
func refuseBackup(t *testing.T, reason string) {
	t.Helper()
	saved := repairBackupFn
	repairBackupFn = func(string) (string, string) { return "", reason }
	t.Cleanup(func() { repairBackupFn = saved })
}

func TestBackupRefusalHardStopsTheRepair(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	corruptDuplicateFTS(t, db)
	original := fileBytes(t, db)
	refuseBackup(t, "a connection to it is still open in this process")
	report := repairReport(t, db, true)
	if report.Repaired || report.BackupPath != "" || !strings.Contains(report.Error, "backup refused") || !strings.Contains(report.Error, "still open") {
		t.Fatalf("report %+v", report)
	}
	if !bytes.Equal(fileBytes(t, db), original) {
		t.Fatal("the damaged source bytes changed")
	}
	if !probeSick(t, db) {
		t.Fatal("a mutating strategy ran")
	}
}

func TestBackupCopyFailureHardStopsTheRepair(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	buildHealthyDB(t, db)
	corruptDuplicateFTS(t, db)
	refuseBackup(t, "backup copy failed: [Errno 28] No space left on device")
	report := repairReport(t, db, true)
	if report.Repaired || !strings.Contains(report.Error, "No space left on device") {
		t.Fatalf("report %+v", report)
	}
	if backups := globNames(t, dir, "state.db.malformed-backup-*"); len(backups) != 0 {
		t.Fatalf("backups %v", backups)
	}
}

func TestBackupFalseStillSkipsBackupAndRepairs(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	buildHealthyDB(t, db)
	corruptDuplicateFTS(t, db)
	report := repairReport(t, db, false)
	if !report.Repaired || report.BackupPath != "" {
		t.Fatalf("report %+v", report)
	}
	if backups := globNames(t, dir, "state.db.malformed-backup-*"); len(backups) != 0 {
		t.Fatalf("backups %v", backups)
	}
}

// Healthy WAL configured store downgraded on disk to delete and then schema corrupted
func downgradedCorruptStore(t *testing.T) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	rawExecAll(t, db, "PRAGMA journal_mode=DELETE")
	corruptDuplicateFTS(t, db)
	return db
}

func TestRepairRestoresConfiguredWalAfterSurgery(t *testing.T) {
	configureRepairJournalMode(t, "wal")
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	if modeOf(t, db) != "wal" {
		t.Fatal("fixture precondition: store not in WAL")
	}
	rawExecAll(t, db, "PRAGMA journal_mode=DELETE")
	if modeOf(t, db) != "delete" {
		t.Fatal("fixture precondition: downgrade failed")
	}
	corruptDuplicateFTS(t, db)
	if report := repairReport(t, db, true); !report.Repaired {
		t.Fatalf("report %+v", report)
	}
	if mode := modeOf(t, db); mode != "wal" {
		t.Fatalf("journal mode %q", mode)
	}
}

func TestRepairRestoreMatchesCanonicalOnVulnerableSqlite(t *testing.T) {
	configureRepairJournalMode(t, "wal")
	walResetVulnerable = func() bool { return true }
	db := downgradedCorruptStore(t)
	if report := repairReport(t, db, true); !report.Repaired {
		t.Fatalf("report %+v", report)
	}
	if mode := modeOf(t, db); mode != "delete" {
		t.Fatalf("journal mode %q", mode)
	}
}

func TestRepairReportsModeChangeWhenProbeSucceeded(t *testing.T) {
	configureRepairJournalMode(t, "wal")
	db := downgradedCorruptStore(t)
	saved := repairJournalProbeFn
	repairJournalProbeFn = func(context.Context, string) string { return "delete" }
	t.Cleanup(func() { repairJournalProbeFn = saved })
	report := repairReport(t, db, true)
	if !report.Repaired || report.JournalModeBefore != "delete" {
		t.Fatalf("report %+v", report)
	}
	if mode := modeOf(t, db); mode != "wal" {
		t.Fatalf("journal mode %q", mode)
	}
}

func TestRepairRestoreFailureIsNonfatal(t *testing.T) {
	configureRepairJournalMode(t, "wal")
	db := downgradedCorruptStore(t)
	saved := applyWALFn
	applyWALFn = func(context.Context, SQLiteConn, string, bool) (string, error) {
		return "", &SQLiteError{Class: SQLiteOperationalError, Msg: "database is locked"}
	}
	t.Cleanup(func() { applyWALFn = saved })
	if report := repairReport(t, db, true); !report.Repaired {
		t.Fatalf("report %+v", report)
	}
	if mode := modeOf(t, db); mode != "delete" {
		t.Fatalf("journal mode %q", mode)
	}
}

func TestRepairHonorsConfiguredDeleteMode(t *testing.T) {
	configureRepairJournalMode(t, "delete")
	db := filepath.Join(t.TempDir(), "state.db")
	buildHealthyDB(t, db)
	if modeOf(t, db) != "delete" {
		t.Fatal("fixture precondition: store not in delete mode")
	}
	corruptDuplicateFTS(t, db)
	if report := repairReport(t, db, true); !report.Repaired {
		t.Fatalf("report %+v", report)
	}
	if mode := modeOf(t, db); mode != "delete" {
		t.Fatalf("journal mode %q", mode)
	}
}

func TestFtsRecoveryIncludesVtablesThatRaiseDecodeErrors(t *testing.T) {
	ctx := context.Background()
	db := usageOpen(t, filepath.Join(t.TempDir(), "state.db"))
	readSession(t, db, NewSession{ID: "needle", Source: "cli"})
	readAppend(t, db, "needle", map[string]any{"role": "user", "content": "searchable needle"})
	saved := ftsProbeExec
	ftsProbeExec = func(ctx context.Context, conn SQLiteConn, table string) error {
		if table == "messages_fts_trigram" {
			return &SQLiteError{Class: SQLiteOperationalError, Msg: "Could not decode to UTF-8 column 'content' with text"}
		}
		return saved(ctx, conn, table)
	}
	t.Cleanup(func() { ftsProbeExec = saved })
	recovered, err := db.sessionSchema.recoverStaleFTS(ctx, db.conn, false, nil)
	if err != nil || !recovered {
		t.Fatalf("recovered %v %v", recovered, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	names := fixtureNames(t, db.path, "SELECT name FROM sqlite_master WHERE name LIKE 'messages_fts%'")
	if !names["messages_fts"] || !names["messages_fts_trigram"] {
		t.Fatalf("fts tables %v", names)
	}
}
