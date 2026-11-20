package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Damaged store of a SQLite header followed by random bytes
func damagedDB(t *testing.T, size int) string {
	t.Helper()
	return makeUnrepairableDB(t, t.TempDir(), size)
}

// Rewrites the damaged store with fresh random bytes of the given size
func rewriteDamaged(t *testing.T, path string, size int) {
	t.Helper()
	body := make([]byte, size)
	rand.Read(body)
	if err := os.WriteFile(path, append([]byte("SQLite format 3\x00"), body...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Touches the file's mtime after a short pause
func touchLater(t *testing.T, path string) {
	t.Helper()
	time.Sleep(10 * time.Millisecond)
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
}

// Fingerprint of a damaged store, failing when the read is refused
func fingerprintOf(t *testing.T, path string) string {
	t.Helper()
	fp, ok := dbFingerprint(path)
	if !ok {
		t.Fatalf("fingerprint of %s refused", path)
	}
	return fp
}

// Whether the attempt budget of a store is exhausted
func exhaustedOf(t *testing.T, path string) bool {
	t.Helper()
	exhausted, err := persistentRepairAttemptsExhausted(path)
	if err != nil {
		t.Fatal(err)
	}
	return exhausted
}

// Disk usage with four hundred gigabytes free
func roomyDisk(t *testing.T) { fakeDiskUsage(t, 500_000_000_000, 400_000_000_000) }

func TestFingerprintSurvivesMtimeChange(t *testing.T) {
	db := damagedDB(t, 200_000)
	before := fingerprintOf(t, db)
	touchLater(t, db)
	if fingerprintOf(t, db) != before {
		t.Fatal("a touched but unchanged file lost its identity")
	}
}

func TestFingerprintChangesWhenContentsChange(t *testing.T) {
	db := damagedDB(t, 200_000)
	before := fingerprintOf(t, db)
	rewriteDamaged(t, db, 200_000)
	if fingerprintOf(t, db) == before {
		t.Fatal("new contents kept the old fingerprint")
	}
}

func TestFingerprintChangesOnTruncation(t *testing.T) {
	db := damagedDB(t, 200_000)
	before := fingerprintOf(t, db)
	if err := os.Truncate(db, 1024); err != nil {
		t.Fatal(err)
	}
	if fingerprintOf(t, db) == before {
		t.Fatal("truncation kept the old fingerprint")
	}
}

func TestAttemptBudgetExhaustsDespiteMtimeChurn(t *testing.T) {
	db := damagedDB(t, 200_000)
	for range maxPersistentRepairAttempts {
		if exhaustedOf(t, db) {
			t.Fatal("budget exhausted early")
		}
		recordRepairOutcome(db, false)
		touchLater(t, db)
	}
	if !exhaustedOf(t, db) {
		t.Fatal("mtime churn kept the budget alive")
	}
}

func TestSuccessfulRepairClearsBudget(t *testing.T) {
	db := damagedDB(t, 200_000)
	for range maxPersistentRepairAttempts {
		recordRepairOutcome(db, false)
	}
	if !exhaustedOf(t, db) {
		t.Fatal("budget not exhausted")
	}
	recordRepairOutcome(db, true)
	if exhaustedOf(t, db) {
		t.Fatal("a successful repair left the budget exhausted")
	}
}

func TestBackupDedupesAcrossMtimeChange(t *testing.T) {
	db := damagedDB(t, 200_000)
	first, reason := backupDBFile(db)
	if first == "" {
		t.Fatal(reason)
	}
	for range 5 {
		touchLater(t, db)
		again, reason := backupDBFile(db)
		if reason != "" || again != first {
			t.Fatalf("a touched but identical DB was copied again: %q %q", again, reason)
		}
	}
	if got := existingMalformedBackups(db); len(got) != 1 {
		t.Fatalf("backups %v", got)
	}
}

func TestBackupRetentionCapStillHolds(t *testing.T) {
	db := damagedDB(t, 200_000)
	for range maxMalformedBackups + 3 {
		rewriteDamaged(t, db, 200_000)
		backupDBFile(db)
	}
	if got := existingMalformedBackups(db); len(got) > maxMalformedBackups {
		t.Fatalf("backups %v", got)
	}
}

func TestBackupRefusedWhenDiskWouldBeExhausted(t *testing.T) {
	db := damagedDB(t, 200_000)
	fakeDiskUsage(t, 10_000_000_000, repairBackupMinFreeBytes/2)
	path, reason := backupDBFile(db)
	if path != "" || !strings.Contains(strings.ToLower(reason), "free") {
		t.Fatalf("path %q reason %q", path, reason)
	}
	if got := existingMalformedBackups(db); len(got) != 0 {
		t.Fatalf("backups %v", got)
	}
}

func TestBackupAllowedOnSmallVolumeWithRoom(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	f, err := os.Create(db)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("SQLite format 3\x00"))
	if err := f.Truncate(50_000_000); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if st, _ := os.Stat(db); st.Size() != 50_000_000 {
		t.Fatalf("size %d", st.Size())
	}
	fakeDiskUsage(t, 10_000_000_000, 1_500_000_000)
	path, reason := backupDBFile(db)
	if reason != "" || path == "" {
		t.Fatalf("path %q reason %q", path, reason)
	}
}

func TestHeadroomScalesWithVolumeSize(t *testing.T) {
	if repairBackupHeadroomBytes(1_000_000_000) != repairBackupMinFreeBytes || repairBackupHeadroomBytes(1_000_000_000_000) <= repairBackupMinFreeBytes {
		t.Fatalf("headroom %d %d", repairBackupHeadroomBytes(1_000_000_000), repairBackupHeadroomBytes(1_000_000_000_000))
	}
}

func TestDiskGuardAccountsForSidecars(t *testing.T) {
	db := damagedDB(t, 1_000_000)
	wal := db + "-wal"
	f, err := os.Create(wal)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(400_000_000); err != nil {
		t.Fatal(err)
	}
	f.Close()
	fakeDiskUsage(t, 10_000_000_000, repairBackupMinFreeBytes+300_000_000)
	path, reason := backupDBFile(db)
	if path != "" || reason == "" {
		t.Fatalf("sidecar bytes were ignored by the free-space check: %q %q", path, reason)
	}
}

// ENOSPC error for a sidecar copy, the shape a full disk returns
func noSpaceError(path string) error {
	return &os.PathError{Op: "copy", Path: path, Err: syscall.ENOSPC}
}

func TestFailedCopyLeavesNoCountableDebris(t *testing.T) {
	dir := t.TempDir()
	db := makeUnrepairableDB(t, dir, 1_000_000)
	body := make([]byte, 1_000_000)
	rand.Read(body)
	if err := os.WriteFile(db+"-wal", body, 0o644); err != nil {
		t.Fatal(err)
	}
	roomyDisk(t)
	realCopy := copyFileFn
	copyFileFn = func(src, dst string) error {
		if strings.HasSuffix(src, "-wal") {
			os.WriteFile(dst, bytes.Repeat([]byte("PARTIAL"), 100), 0o644)
			return noSpaceError(dst)
		}
		return realCopy(src, dst)
	}
	for range 6 {
		backupDBFile(db)
		touchLater(t, db)
	}
	copyFileFn = realCopy
	if got := existingMalformedBackups(db); len(got) > maxMalformedBackups {
		t.Fatalf("backups %v", got)
	}
	path, reason := backupDBFile(db)
	if reason != "" || path == "" {
		t.Fatalf("path %q reason %q", path, reason)
	}
	strays := append(globNames(t, dir, "*.backup-staging-*"), globNames(t, dir, "*.incomplete*")...)
	if len(strays) != 0 {
		t.Fatalf("staging debris survived: %v", strays)
	}
}

func TestBackupAllowedWithAmpleDisk(t *testing.T) {
	db := damagedDB(t, 200_000)
	fakeDiskUsage(t, 0, repairBackupMinFreeBytes*10)
	path, reason := backupDBFile(db)
	if reason != "" || path == "" {
		t.Fatalf("path %q reason %q", path, reason)
	}
}

func TestRepairAbortsWhenBackupRefusedForDisk(t *testing.T) {
	db := damagedDB(t, 200_000)
	fakeDiskUsage(t, 0, repairBackupMinFreeBytes/2)
	report := repairReport(t, db, true)
	if report.Repaired || !strings.Contains(strings.ToLower(report.Error), "free") {
		t.Fatalf("report %+v", report)
	}
}

// Descriptors of this process open on the path, read from procfs
func openDescriptorsOf(t *testing.T, path string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("procfs unavailable")
	}
	n := 0
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err == nil && target == path {
			n++
		}
	}
	return n
}

// Plain database with one table, created and closed
func plainTableDB(t *testing.T) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "state.db")
	rawExecAll(t, db, "CREATE TABLE t(a)")
	return db
}

func TestFingerprintTakesNoRawFdWhileAConnectionIsLive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs descriptor listing")
	}
	db := plainTableDB(t)
	live, err := ConnectTracked(db, WithTrackingPath(db))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	before := openDescriptorsOf(t, db)
	fp, ok := dbFingerprint(db)
	if ok {
		t.Fatalf("a live connection produced a fingerprint %q", fp)
	}
	if after := openDescriptorsOf(t, db); after != before {
		t.Fatalf("raw fd taken on a live DB (%d -> %d)", before, after)
	}
}

func TestLiveConnectionKeepsItsWriteLockAcrossARepairPass(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX advisory locks")
	}
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "state.db")
	rawExecAll(t, db, "PRAGMA journal_mode=DELETE", "CREATE TABLE sessions(id TEXT)")
	peerCanWrite := func() bool {
		out := runHelper(t, spawnHelper(t, "peer-write", db))
		if out != "WROTE" && out != "BLOCKED" {
			t.Fatalf("unexpected peer output %q", out)
		}
		return out == "WROTE"
	}
	live, err := ConnectTracked(db, WithTrackingPath(db))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	execAll(t, live, "BEGIN IMMEDIATE", "INSERT INTO sessions VALUES('holder')")
	if peerCanWrite() {
		t.Fatal("peer wrote before the fingerprint (bad fixture)")
	}
	dbFingerprint(db)
	if peerCanWrite() {
		t.Fatal("the fingerprint cancelled the holder's POSIX advisory lock")
	}
	if _, err := live.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
}

func TestStagingNameIsOutsideTheBackupPrefix(t *testing.T) {
	db := damagedDB(t, 20_000)
	roomyDisk(t)
	realCopy := copyFileFn
	var stagingNames []string
	copyFileFn = func(src, dst string) error {
		stagingNames = append(stagingNames, filepath.Base(dst))
		return realCopy(src, dst)
	}
	t.Cleanup(func() { copyFileFn = realCopy })
	path, reason := backupDBFile(db)
	if reason != "" || path == "" || len(stagingNames) == 0 {
		t.Fatalf("path %q reason %q staging %v", path, reason, stagingNames)
	}
	prefix := filepath.Base(db) + ".malformed-backup-"
	for _, name := range stagingNames {
		if strings.HasPrefix(name, prefix) {
			t.Fatalf("staging name %q matches the backup prefix", name)
		}
	}
}

func TestOrphanedStagingIsNeverReturnedAsTheBackupPath(t *testing.T) {
	db := damagedDB(t, 20_000)
	roomyDisk(t)
	realCopy := copyFileFn
	var seen []string
	copyFileFn = func(src, dst string) error {
		seen = append(seen, dst)
		return realCopy(src, dst)
	}
	first, _ := backupDBFile(db)
	copyFileFn = realCopy
	if first == "" || len(seen) == 0 {
		t.Fatal("no backup was made (fixture problem)")
	}
	os.Remove(first)
	orphan := seen[0]
	if err := realCopy(db, orphan); err != nil {
		t.Fatal(err)
	}
	path, reason := backupDBFile(db)
	if reason != "" || path == "" || path == orphan || strings.HasSuffix(path, ".incomplete") || strings.Contains(filepath.Base(path), "staging") {
		t.Fatalf("path %q reason %q", path, reason)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("stale staging debris was not swept")
	}
}

func TestBackupRefusedWhenFreeSpaceCannotBeDetermined(t *testing.T) {
	db := damagedDB(t, 200_000)
	saved := diskUsageFn
	diskUsageFn = func(string) (uint64, uint64, uint64, error) { return 0, 0, 0, errors.New("statvfs failed") }
	t.Cleanup(func() { diskUsageFn = saved })
	path, reason := backupDBFile(db)
	if path != "" || !strings.Contains(strings.ToLower(reason), "free space") {
		t.Fatalf("path %q reason %q", path, reason)
	}
	if got := existingMalformedBackups(db); len(got) != 0 {
		t.Fatalf("backups %v", got)
	}
}

func TestBudgetExhaustsWhenLivenessAlternatesAcrossPasses(t *testing.T) {
	db := plainTableDB(t)
	for index := range maxPersistentRepairAttempts {
		var live *TrackedConn
		if index%2 == 1 {
			var err error
			if live, err = ConnectTracked(db, WithTrackingPath(db)); err != nil {
				t.Fatal(err)
			}
		}
		if exhaustedOf(t, db) {
			t.Fatalf("budget exhausted at pass %d", index)
		}
		recordRepairOutcome(db, false)
		if live != nil {
			live.Close()
		}
	}
	if !exhaustedOf(t, db) {
		t.Fatal("alternating live/offline passes reset the repair budget")
	}
	live, err := ConnectTracked(db, WithTrackingPath(db))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if !exhaustedOf(t, db) {
		t.Fatal("an exhausted budget hid behind a live connection")
	}
}

func TestFingerprintReturnsNoneRatherThanAMtimeShapedKey(t *testing.T) {
	db := plainTableDB(t)
	fingerprintOf(t, db)
	live, err := ConnectTracked(db, WithTrackingPath(db))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if fp, ok := dbFingerprint(db); ok {
		t.Fatalf("a live connection produced a fingerprint %q", fp)
	}
}

// Database larger than the fingerprint sample window in the given journal mode
func populatedDB(t *testing.T, path, journalMode string) {
	t.Helper()
	ctx := context.Background()
	conn := openRaw(t, path)
	execAll(t, conn, "PRAGMA journal_mode="+journalMode, "CREATE TABLE sessions(id TEXT, blob TEXT)")
	for i := range 600 {
		if _, err := conn.ExecContext(ctx, "INSERT INTO sessions VALUES(?,?)", fmt.Sprint(i), strings.Repeat("x", 400)); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
}

func TestOrdinaryCommitDoesNotRekeyTheFingerprint(t *testing.T) {
	for _, mode := range []string{"DELETE", "WAL"} {
		db := filepath.Join(t.TempDir(), "state_"+mode+".db")
		populatedDB(t, db, mode)
		before := fingerprintOf(t, db)
		rawExecAll(t, db, "UPDATE sessions SET blob='peer' WHERE id='20000'")
		if fingerprintOf(t, db) != before {
			t.Fatalf("%s mode: an ordinary commit re-keyed the ledger", mode)
		}
	}
}

func TestBudgetExhaustsWhileAWriterCommitsBetweenPasses(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	populatedDB(t, db, "DELETE")
	for index := range maxPersistentRepairAttempts {
		if exhaustedOf(t, db) {
			t.Fatalf("budget exhausted at pass %d", index)
		}
		recordRepairOutcome(db, false)
		conn := openRaw(t, db)
		if _, err := conn.ExecContext(context.Background(), "UPDATE sessions SET blob=? WHERE id='20000'", fmt.Sprintf("v%d", index)); err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	if !exhaustedOf(t, db) {
		t.Fatal("a live writer's commits reset the repair budget every pass")
	}
}

func TestGenuineRecoveryStillResetsTheBudget(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	populatedDB(t, db, "DELETE")
	for _, c := range []struct{ label, sql string }{
		{"sqlite_master rewrite", "CREATE TABLE healed(x)"},
		{"index rebuild", "CREATE INDEX ix_sessions_id ON sessions(id)"},
		{"VACUUM", "VACUUM"},
	} {
		before := fingerprintOf(t, db)
		rawExecAll(t, db, c.sql)
		if fingerprintOf(t, db) == before {
			t.Fatalf("%s left the fingerprint unchanged", c.label)
		}
	}
	before := fingerprintOf(t, db)
	if err := os.Truncate(db, 4096); err != nil {
		t.Fatal(err)
	}
	if fingerprintOf(t, db) == before {
		t.Fatal("truncation left the fingerprint unchanged")
	}
}

func TestForensicBackupIncludesTheRollbackJournal(t *testing.T) {
	db := damagedDB(t, 20_000)
	for suffix, payload := range map[string]string{"-wal": "WALDATA", "-journal": "JOURNALDATA"} {
		if err := os.WriteFile(db+suffix, []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	roomyDisk(t)
	path, reason := backupDBFile(db)
	if reason != "" || path == "" {
		t.Fatalf("path %q reason %q", path, reason)
	}
	if got := fileBytes(t, path+"-journal"); string(got) != "JOURNALDATA" {
		t.Fatalf("journal copy %q", got)
	}
	if got := fileBytes(t, path+"-wal"); string(got) != "WALDATA" {
		t.Fatalf("wal copy %q", got)
	}
	if got := existingMalformedBackups(db); len(got) != 1 {
		t.Fatalf("backups %v", got)
	}
}

func TestPruneRemovesJournalSidecarsToo(t *testing.T) {
	dir := t.TempDir()
	db := makeUnrepairableDB(t, dir, 20_000)
	if err := os.WriteFile(db+"-journal", []byte("J"), 0o644); err != nil {
		t.Fatal(err)
	}
	roomyDisk(t)
	for range maxMalformedBackups + 2 {
		rewriteDamaged(t, db, 20_000)
		backupDBFile(db)
	}
	kept := existingMalformedBackups(db)
	if len(kept) > maxMalformedBackups {
		t.Fatalf("backups %v", kept)
	}
	keptNames := map[string]bool{}
	for _, p := range kept {
		keptNames[filepath.Base(p)] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, "-journal") && strings.Contains(name, ".malformed-backup-") && !keptNames[strings.TrimSuffix(name, "-journal")] {
			t.Fatalf("pruned backup left its journal behind: %s", name)
		}
	}
	for _, survivor := range kept {
		if _, err := os.Stat(survivor + "-journal"); err != nil {
			t.Fatalf("surviving backup %s lost its journal", survivor)
		}
	}
}

func TestBackupNotDedupedAfterInteriorPageWrite(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	rewriteDamaged(t, db, 300_000)
	st, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	size := st.Size()
	roomyDisk(t)
	first, reason := backupDBFile(db)
	if reason != "" || first == "" {
		t.Fatalf("path %q reason %q", first, reason)
	}
	raw := fileBytes(t, db)
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(db, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(db, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(db); after.Size() != size {
		t.Fatalf("size changed %d", after.Size())
	}
	srcID, _ := backupContentIdentity(db)
	firstID, _ := backupContentIdentity(first)
	if srcID == firstID {
		t.Fatal("the backup content identity is blind to the interior change")
	}
	second, reason := backupDBFile(db)
	if reason != "" || second == "" || second == first {
		t.Fatalf("an interior-page write was wrongly deduped to a stale backup: %q %q", second, reason)
	}
	if got := existingMalformedBackups(db); len(got) != 2 {
		t.Fatalf("backups %v", got)
	}
}

func TestPublicationFailureLeavesNoCountablePartialBundle(t *testing.T) {
	dir := t.TempDir()
	db := makeUnrepairableDB(t, dir, 200_000)
	body := make([]byte, 50_000)
	rand.Read(body)
	if err := os.WriteFile(db+"-wal", body, 0o644); err != nil {
		t.Fatal(err)
	}
	roomyDisk(t)
	realReplace := replaceFileFn
	calls := 0
	replaceFileFn = func(src, dst string) error {
		calls++
		if calls == 2 {
			return noSpaceError(dst)
		}
		return realReplace(src, dst)
	}
	backupDBFile(db)
	replaceFileFn = realReplace
	if got := existingMalformedBackups(db); len(got) != 0 {
		t.Fatalf("a partial bundle was left countable %v", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".malformed-backup-") && e.Name() != "state.db" {
			t.Fatalf("partial promoted file survived: %s", e.Name())
		}
	}
	path, reason := backupDBFile(db)
	if reason != "" || path == "" {
		t.Fatalf("path %q reason %q", path, reason)
	}
	if strays := globNames(t, dir, "*.backup-staging-*"); len(strays) != 0 {
		t.Fatalf("staging debris survived: %v", strays)
	}
}
