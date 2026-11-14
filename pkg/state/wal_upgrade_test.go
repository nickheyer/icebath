package state

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Creates a DELETE mode database that already holds a row
func makeDeleteDBWithContent(t *testing.T, path string) {
	t.Helper()
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if row, err := fetchOne(context.Background(), conn, "PRAGMA journal_mode=DELETE"); err != nil || modeFromRow(row) != "delete" {
		t.Fatalf("seed DELETE = %v, %v", row, err)
	}
	mustExecConn(t, conn, "CREATE TABLE t (x)")
	mustExecConn(t, conn, "INSERT INTO t VALUES (1)")
}

// Applies the journal policy to a fresh connection on path and returns the mode
func applyOn(t *testing.T, path, label string) string {
	t.Helper()
	conn := openWALTestConn(t, path)
	mode, err := ApplyWALWithFallback(context.Background(), conn, label, false)
	if err != nil {
		t.Fatal(err)
	}
	return mode
}

// Configures a journal mode with the vulnerability gate pinned and dedupe sets cleared
func upgradeFixture(t *testing.T, mode string, vulnerable bool) *syncBuffer {
	t.Helper()
	resetWALOnceState(t)
	configureJournalMode(t, mode)
	pinVar(t, &walResetVulnerable, func() bool { return vulnerable })
	return captureSlog(t)
}

func TestAnExistingDeleteDatabaseWarnsWhenFlipped(t *testing.T) {
	buf := upgradeFixture(t, "wal", false)
	path := filepath.Join(t.TempDir(), "existing-delete.db")
	makeDeleteDBWithContent(t, path)
	if mode := applyOn(t, path, "state.db"); mode != "wal" {
		t.Fatalf("mode = %q", mode)
	}
	lines := logLinesWith(buf, "WARN", "state.db: on-disk journal_mode was delete and has been switched to WAL")
	if len(lines) != 1 {
		t.Fatalf("upgrade warnings = %s", buf.String())
	}
}

func TestTheFlipStillHappens(t *testing.T) {
	upgradeFixture(t, "wal", false)
	path := filepath.Join(t.TempDir(), "existing-delete.db")
	makeDeleteDBWithContent(t, path)
	conn := openWALTestConn(t, path)
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "state.db", false); err != nil || mode != "wal" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if journalModeOf(t, conn) != "wal" {
		t.Fatal("database not flipped to WAL")
	}
}

func TestUpgradeWarningFiresOncePerProcessPerDatabase(t *testing.T) {
	buf := upgradeFixture(t, "wal", false)
	dir := t.TempDir()
	for _, name := range []string{"a", "b"} {
		path := filepath.Join(dir, name+".db")
		makeDeleteDBWithContent(t, path)
		applyOn(t, path, "same-label.db")
	}
	if hits := logLinesWith(buf, "", "same-label.db"); len(hits) != 1 {
		t.Fatalf("expected one deduped warning, got %d", len(hits))
	}
}

func TestASecondDatabaseGetsItsOwnWarning(t *testing.T) {
	buf := upgradeFixture(t, "wal", false)
	dir := t.TempDir()
	for _, label := range []string{"state.db", "kanban.db"} {
		path := filepath.Join(dir, label)
		makeDeleteDBWithContent(t, path)
		applyOn(t, path, label)
	}
	blob := buf.String()
	if !strings.Contains(blob, "state.db: on-disk") || !strings.Contains(blob, "kanban.db: on-disk") {
		t.Fatalf("per database warnings missing: %s", blob)
	}
}

func TestABrandNewDatabaseIsSilent(t *testing.T) {
	buf := upgradeFixture(t, "wal", false)
	if mode := applyOn(t, filepath.Join(t.TempDir(), "fresh.db"), "fresh.db"); mode != "wal" {
		t.Fatalf("mode = %q", mode)
	}
	if strings.Contains(buf.String(), "fresh.db") {
		t.Fatalf("fresh database logged: %s", buf.String())
	}
}

func TestAnExistingWALDatabaseIsSilent(t *testing.T) {
	buf := upgradeFixture(t, "wal", false)
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "already-wal.db"))
	if _, err := fetchOne(context.Background(), conn, "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, conn, "CREATE TABLE t (x)")
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "already-wal.db", false); err != nil || mode != "wal" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if strings.Contains(buf.String(), "already-wal.db") {
		t.Fatalf("existing WAL logged: %s", buf.String())
	}
}

func TestConfiguredDeleteIsSilent(t *testing.T) {
	buf := upgradeFixture(t, "delete", false)
	path := filepath.Join(t.TempDir(), "configured-delete.db")
	makeDeleteDBWithContent(t, path)
	conn := openWALTestConn(t, path)
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "configured-delete.db", false); err != nil || mode != "delete" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if journalModeOf(t, conn) != "delete" {
		t.Fatal("configured delete not applied")
	}
	if strings.Contains(buf.String(), "configured-delete.db") {
		t.Fatalf("configured delete logged: %s", buf.String())
	}
}

func TestTheWALResetVulnerablePathIsSilent(t *testing.T) {
	buf := upgradeFixture(t, "wal", true)
	path := filepath.Join(t.TempDir(), "vulnerable.db")
	makeDeleteDBWithContent(t, path)
	if mode := applyOn(t, path, "vulnerable.db"); mode != "delete" {
		t.Fatalf("mode = %q", mode)
	}
	if strings.Contains(buf.String(), "database.journal_mode") {
		t.Fatalf("vulnerable path mentioned journal_mode config: %s", buf.String())
	}
}

func TestOnDiskWALIsStillNeverLiveDowngraded(t *testing.T) {
	upgradeFixture(t, "delete", false)
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "existing-wal.db"))
	if row, err := fetchOne(context.Background(), conn, "PRAGMA journal_mode=WAL"); err != nil || modeFromRow(row) != "wal" {
		t.Fatalf("seed WAL = %v, %v", row, err)
	}
	walResetVulnerable = func() bool { return true }
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "existing-wal.db", false); err != nil || mode != "wal" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if journalModeOf(t, conn) != "wal" {
		t.Fatal("on disk WAL downgraded")
	}
}

func TestDefaultConfigStillYieldsWALOnAFreshDatabase(t *testing.T) {
	upgradeFixture(t, "wal", false)
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "default.db"))
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "default.db", false); err != nil || mode != "wal" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if journalModeOf(t, conn) != "wal" {
		t.Fatal("fresh database not WAL")
	}
}
