package state

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func makeUnrepairableDB(t *testing.T, dir string, n int) string {
	t.Helper()
	db := filepath.Join(dir, "state.db")
	body := make([]byte, n)
	rand.Read(body)
	if err := os.WriteFile(db, append([]byte("SQLite format 3\x00"), body...), 0o644); err != nil {
		t.Fatal(err)
	}
	return db
}

func ledgerAttempts(t *testing.T, db string) float64 {
	t.Helper()
	data, err := os.ReadFile(repairLedgerPath(db))
	if err != nil {
		t.Fatal(err)
	}
	var ledger map[string]any
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	n, _ := ledger["failed_attempts"].(float64)
	return n
}

func mustRepair(t *testing.T, db string) *RepairReport {
	t.Helper()
	report, err := RepairStateDBSchema(context.Background(), db, true)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestFailedRepairsAccumulateInLedger(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := makeUnrepairableDB(t, t.TempDir(), 4096)
	if report := mustRepair(t, db); report.Repaired {
		t.Fatalf("garbage must not repair: %+v", report)
	}
	if n := ledgerAttempts(t, db); n != 1 {
		t.Fatalf("failed_attempts %v", n)
	}
}

func TestRepairRefusesAfterCapWithTerminalError(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := makeUnrepairableDB(t, t.TempDir(), 4096)
	for range maxPersistentRepairAttempts {
		if mustRepair(t, db).Repaired {
			t.Fatal("garbage must not repair")
		}
	}
	before := len(existingMalformedBackups(db))
	report := mustRepair(t, db)
	if report.Repaired {
		t.Fatal("exhausted budget must refuse")
	}
	for _, want := range []string{"Manual recovery required", ".recover", "sessions recover", "sqlite3"} {
		if !strings.Contains(report.Error, want) {
			t.Fatalf("terminal error lacks %q: %s", want, report.Error)
		}
	}
	if strings.Contains(report.Error, `sqlite3 state.db ".recover"`) {
		t.Fatal("terminal error must not point a raw sqlite3 shell at the live database")
	}
	if len(existingMalformedBackups(db)) != before || ledgerAttempts(t, db) != maxPersistentRepairAttempts {
		t.Fatal("refusal must run no surgery and take no backup")
	}
}

func TestChangedFileResetsTheBudget(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	db := makeUnrepairableDB(t, dir, 4096)
	for range maxPersistentRepairAttempts {
		mustRepair(t, db)
	}
	if exhausted, err := persistentRepairAttemptsExhausted(db); err != nil || !exhausted {
		t.Fatalf("budget must be burned: %v %v", exhausted, err)
	}
	makeUnrepairableDB(t, dir, 8192)
	if exhausted, _ := persistentRepairAttemptsExhausted(db); exhausted {
		t.Fatal("a replaced file must get fresh attempts")
	}
}

func TestSuccessfulRepairClearsTheLedger(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	makeRepairDB(t, db)
	recordRepairOutcome(db, false)
	if _, err := os.Stat(repairLedgerPath(db)); err != nil {
		t.Fatal("failure must write the ledger")
	}
	recordRepairOutcome(db, true)
	if _, err := os.Stat(repairLedgerPath(db)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("success must clear the ledger")
	}
}

func TestCorruptLedgerIsIgnoredNotFatal(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := makeUnrepairableDB(t, t.TempDir(), 4096)
	os.WriteFile(repairLedgerPath(db), []byte("{not json"), 0o644)
	if exhausted, err := persistentRepairAttemptsExhausted(db); err != nil || exhausted {
		t.Fatalf("corrupt ledger must read as not exhausted: %v %v", exhausted, err)
	}
	mustRepair(t, db)
	if n := ledgerAttempts(t, db); n != 1 {
		t.Fatalf("repair must overwrite the corrupt ledger, failed_attempts %v", n)
	}
}

func TestIdenticalFileIsNotBackedUpTwice(t *testing.T) {
	db := makeUnrepairableDB(t, t.TempDir(), 4096)
	first, reason := backupDBFile(db)
	second, reason2 := backupDBFile(db)
	if first == "" || reason != "" || reason2 != "" || second != first || len(existingMalformedBackups(db)) != 1 {
		t.Fatalf("first %q second %q reasons %q %q", first, second, reason, reason2)
	}
}

func TestChangedFileGetsANewBackup(t *testing.T) {
	dir := t.TempDir()
	db := makeUnrepairableDB(t, dir, 4096)
	first, _ := backupDBFile(db)
	makeUnrepairableDB(t, dir, 2048)
	second, reason := backupDBFile(db)
	if reason != "" || second == first || second == "" {
		t.Fatalf("changed bytes need a fresh backup, got %q %q", second, reason)
	}
}

func TestRetentionCapPrunesOldest(t *testing.T) {
	dir := t.TempDir()
	db := makeUnrepairableDB(t, dir, 4096)
	for i := range maxMalformedBackups + 3 {
		fake := fmt.Sprintf("%s.malformed-backup-20260801_00000%d", db, i)
		os.WriteFile(fake, []byte("x"), 0o644)
		os.WriteFile(fake+"-wal", []byte("w"), 0o644)
	}
	pruneMalformedBackups(db, maxMalformedBackups)
	remaining := existingMalformedBackups(db)
	if len(remaining) != maxMalformedBackups {
		t.Fatalf("kept %v", remaining)
	}
	sorted := slices.Clone(remaining)
	slices.Sort(sorted)
	slices.Reverse(sorted)
	if !slices.Equal(sorted, remaining) {
		t.Fatalf("not newest first %v", remaining)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if p := filepath.Join(dir, e.Name()); strings.HasSuffix(p, "-wal") && !slices.Contains(remaining, strings.TrimSuffix(p, "-wal")) {
			t.Fatalf("sidecar of a pruned backup left behind %s", p)
		}
	}
}

func TestBackupViaRepairPathDoesNotAccumulate(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := makeUnrepairableDB(t, t.TempDir(), 4096)
	for range maxPersistentRepairAttempts {
		mustRepair(t, db)
	}
	if n := len(existingMalformedBackups(db)); n != 1 {
		t.Fatalf("repeated failed repairs kept %d backups", n)
	}
}

func guestConn(t *testing.T, home, configYAML string) SQLiteConn {
	t.Helper()
	t.Setenv("ICEBATH_HOME", home)
	if configYAML != "" {
		os.WriteFile(filepath.Join(home, "config.yaml"), []byte(configYAML), 0o644)
	}
	conn := openRaw(t, filepath.Join(home, "state.db"))
	execAll(t, conn, "PRAGMA journal_mode=DELETE", "PRAGMA synchronous=1")
	return conn
}

func TestGuestBarriersApplyConfiguredSynchronous(t *testing.T) {
	conn := guestConn(t, t.TempDir(), "database:\n  synchronous: FULL\n")
	defer conn.Close()
	ApplyDurabilityBarriers(context.Background(), conn)
	sync, _ := fetchOne(context.Background(), conn, "PRAGMA synchronous")
	mode, _ := fetchOne(context.Background(), conn, "PRAGMA journal_mode")
	if pyStr(sync[0]) != "2" || strings.ToLower(pyStr(mode[0])) != "delete" {
		t.Fatalf("synchronous %v journal_mode %v", sync, mode)
	}
}

func TestGuestBarriersLeaveSynchronousAloneWhenUnset(t *testing.T) {
	conn := guestConn(t, t.TempDir(), "database: {}\n")
	defer conn.Close()
	ApplyDurabilityBarriers(context.Background(), conn)
	if sync, _ := fetchOne(context.Background(), conn, "PRAGMA synchronous"); pyStr(sync[0]) != "1" {
		t.Fatalf("synchronous %v", sync)
	}
}

func TestGuestBarriersSurviveConfigFailure(t *testing.T) {
	conn := guestConn(t, t.TempDir(), "database: [unterminated\n")
	defer conn.Close()
	ApplyDurabilityBarriers(context.Background(), conn)
}
