package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	rootExt4      = "25 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw"
	bindVirtiofs  = "612 25 0:53 / /data rw,relatime shared:300 - fuse.virtiofs mount0 rw"
	bind9P        = "613 25 0:54 / /mnt/host rw,relatime - 9p host0 rw,trans=virtio"
	nestedExt4    = "614 612 8:2 / /data/native rw,relatime - ext4 /dev/sdb1 rw"
	spaceVirtiofs = `615 25 0:55 / /mnt/my\040share rw,relatime - virtiofs share rw`
)

// Writes mountinfo rows newline joined with a trailing newline and returns the path
func writeMountinfo(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOnlyVirtiofsAnd9PMountsAreFlagged(t *testing.T) {
	pinVar(t, &walPlatform, "linux")
	cases := []struct {
		path     string
		expected bool
	}{
		{"/data/agent", true},
		{"/mnt/host/db", true},
		{"/mnt/my share/db", true},
		{"/home/user/.icebath", false},
		{"/data/native/db", false},
		{"/datastore", false},
	}
	for _, c := range cases {
		mi := writeMountinfo(t, rootExt4, bindVirtiofs, bind9P, nestedExt4, spaceVirtiofs)
		got, err := detectCrossVMFS(c.path, mi)
		if err != nil || got != c.expected {
			t.Errorf("detectCrossVMFS(%q) = %v, %v want %v", c.path, got, err, c.expected)
		}
	}
}

func TestOrdinaryFilesystemsNeverFlagged(t *testing.T) {
	pinVar(t, &walPlatform, "linux")
	for _, fstype := range []string{"ext4", "xfs", "btrfs", "zfs", "tmpfs", "overlay", "nfs", "nfs4", "cifs", "fuse.sshfs", "apfs", "f2fs"} {
		mi := writeMountinfo(t, "25 1 8:1 / / rw,relatime shared:1 - "+fstype+" /dev/sda1 rw")
		if got, err := detectCrossVMFS("/home/user/.icebath", mi); err != nil || got {
			t.Errorf("%s flagged: %v %v", fstype, got, err)
		}
	}
}

func TestMissingMountinfoConservativeFalse(t *testing.T) {
	pinVar(t, &walPlatform, "linux")
	if got, err := detectCrossVMFS("/data", filepath.Join(t.TempDir(), "nope")); err != nil || got {
		t.Fatalf("missing mountinfo = %v, %v", got, err)
	}
}

func TestCrossVMDetectionIsLinuxOnly(t *testing.T) {
	pinVar(t, &walPlatform, "darwin")
	mi := writeMountinfo(t, rootExt4, bindVirtiofs)
	if got, err := detectCrossVMFS("/data/agent", mi); err != nil || got {
		t.Fatalf("darwin detection = %v, %v", got, err)
	}
}

func TestMountinfoFstypeEdges(t *testing.T) {
	mi := writeMountinfo(t, "1 2 3:4 / /data rw - VirtioFS src rw", "no separator line here at all", "1 2 / - x")
	if got, err := mountinfoFstype("/data/x", mi); err != nil || got != "virtiofs" {
		t.Fatalf("fstype = %q, %v", got, err)
	}
	mi = writeMountinfo(t, "1 2 3:4 / /data rw -    ")
	if _, err := mountinfoFstype("/data/x", mi); err == nil || !strings.Contains(err.Error(), "IndexError") {
		t.Fatalf("blank fstype tail = %v", err)
	}
	mi = writeMountinfo(t, `1 2 3:4 / /bad\x4 rw - virtiofs src rw`)
	if _, err := mountinfoFstype("/data/x", mi); err == nil || !strings.Contains(err.Error(), `truncated \xXX escape`) {
		t.Fatalf("malformed escape = %v", err)
	}
	if _, err := mountinfoFstype("/data", "/proc/self/mount\x00info"); err == nil || !strings.Contains(err.Error(), "embedded null byte") {
		t.Fatalf("nul path = %v", err)
	}
}

func TestPyUnicodeEscapeDecode(t *testing.T) {
	cases := []struct {
		in, want, err string
	}{
		{`/mnt/my\040share`, "/mnt/my share", ""},
		{`a\tb\\c\'\"\n`, "a\tb\\c'\"\n", ""},
		{`\777`, "\u01ff", ""},
		{`\q`, `\q`, ""},
		{"x\\\ny", "xy", ""},
		{"\\x41\\u00e9\\U0001F600", "A\u00e9\U0001F600", ""},
		{`\N{LATIN SMALL LETTER A}\N{latin small letter b}`, "ab", ""},
		{`\x4`, "", `'unicodeescape' codec can't decode bytes in position 0-2: truncated \xXX escape`},
		{`ab\N{bogus}`, "", `'unicodeescape' codec can't decode bytes in position 2-10: unknown Unicode character name`},
		{`\N{}`, "", `'unicodeescape' codec can't decode bytes in position 0-2: malformed \N character escape`},
		{`\Nx`, "", `'unicodeescape' codec can't decode bytes in position 0-1: malformed \N character escape`},
		{`\U00110000`, "", `'unicodeescape' codec can't decode bytes in position 0-9: illegal Unicode character`},
		{`a\`, "", `'unicodeescape' codec can't decode byte 0x5c in position 1: \ at end of string`},
	}
	for _, c := range cases {
		got, err := pyUnicodeEscapeDecode([]byte(c.in))
		if c.err != "" {
			if err == nil || err.Error() != "UnicodeDecodeError: "+c.err {
				t.Errorf("%q error = %v want %s", c.in, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q = %q, %v want %q", c.in, got, err, c.want)
		}
	}
	if got := latin1Ignore("a\u00e9\u65e5b"); string(got) != "a\xe9b" {
		t.Fatalf("latin1Ignore = %q", got)
	}
}

// Pins the vulnerability gate off and the configured mode to wal as the refusal fixture does
func isolateCrossVM(t *testing.T) {
	resetWALOnceState(t)
	pinVar(t, &walResetVulnerable, func() bool { return false })
	pinVar(t, &resolveJournalMode, func(context.Context) string { return "wal" })
}

func TestFreshDBOnCrossVMFSGetsDeleteAndWithoutDetectionGetsWAL(t *testing.T) {
	isolateCrossVM(t)
	ctx := context.Background()
	dir := t.TempDir()
	pinVar(t, &pathOnCrossVMFS, func(string) (bool, error) { return true, nil })
	conn := openWALTestConn(t, filepath.Join(dir, "a.db"))
	if mode, err := ApplyWALWithFallback(ctx, conn, "a.db", false); err != nil || mode != "delete" {
		t.Fatalf("cross VM fresh db = %q, %v", mode, err)
	}
	pathOnCrossVMFS = func(string) (bool, error) { return false, nil }
	conn = openWALTestConn(t, filepath.Join(dir, "b.db"))
	if mode, err := ApplyWALWithFallback(ctx, conn, "b.db", false); err != nil || mode != "wal" {
		t.Fatalf("native fresh db = %q, %v", mode, err)
	}
}

func TestRequireWALRaisesOnCrossVMFS(t *testing.T) {
	isolateCrossVM(t)
	pinVar(t, &pathOnCrossVMFS, func(string) (bool, error) { return true, nil })
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "state.db"))
	_, err := ApplyWALWithFallback(context.Background(), conn, "state.db", true)
	var unsupported *WALUnsupportedError
	if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "cross-VM") || !isOperationalError(err) {
		t.Fatalf("require WAL on cross VM = %v", err)
	}
}

// Creates a database already in WAL mode holding one table
func seedWALDB(t *testing.T, path string) {
	t.Helper()
	seed, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	ctx := context.Background()
	row, err := fetchOne(ctx, seed, "PRAGMA journal_mode=WAL")
	if err != nil || modeFromRow(row) != "wal" {
		t.Fatalf("seed WAL = %v, %v", row, err)
	}
	if _, err := seed.ExecContext(ctx, "CREATE TABLE t (x)"); err != nil {
		t.Fatal(err)
	}
}

func TestOnDiskWALDBIsNeverDowngraded(t *testing.T) {
	isolateCrossVM(t)
	db := filepath.Join(t.TempDir(), "already-wal.db")
	seedWALDB(t, db)
	pinVar(t, &pathOnCrossVMFS, func(string) (bool, error) { return true, nil })
	conn := openWALTestConn(t, db)
	if mode, err := ApplyWALWithFallback(context.Background(), conn, db, false); err != nil || mode != "wal" {
		t.Fatalf("on disk WAL = %q, %v", mode, err)
	}
}

func TestExistingWALDBOnCrossVMFSWarnsOperatorOnce(t *testing.T) {
	for _, vulnerable := range []bool{false, true} {
		isolateCrossVM(t)
		buf := captureSlog(t)
		walResetVulnerable = func() bool { return vulnerable }
		db := filepath.Join(t.TempDir(), "already-wal.db")
		seedWALDB(t, db)
		pinVar(t, &pathOnCrossVMFS, func(string) (bool, error) { return true, nil })
		for range 2 {
			conn := openWALTestConn(t, db)
			if mode, err := ApplyWALWithFallback(context.Background(), conn, "state.db", false); err != nil || mode != "wal" {
				t.Fatalf("vulnerable=%v mode = %q, %v", vulnerable, mode, err)
			}
			conn.Close()
		}
		if got := logLinesWith(buf, "ERROR", "cross-VM"); len(got) != 1 {
			t.Fatalf("vulnerable=%v cross VM errors = %d", vulnerable, len(got))
		}
	}
}

// Writes a config whose database section sets journal_mode to the given YAML scalar
func configureJournalMode(t *testing.T, yamlValue string) {
	t.Helper()
	writeHomeConfig(t, "database:\n  journal_mode: "+yamlValue+"\n")
}

func TestWALProbeUnknownNeverEmitsSetPragma(t *testing.T) {
	resetWALOnceState(t)
	configureJournalMode(t, "wal")
	pinVar(t, &walResetVulnerable, func() bool { return false })
	buf := captureSlog(t)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "probe-unknown.db")
	sibling := openWALTestConn(t, dbPath)
	if row, err := fetchOne(ctx, sibling, "PRAGMA journal_mode=WAL"); err != nil || modeFromRow(row) != "wal" {
		t.Fatalf("sibling WAL = %v, %v", row, err)
	}
	pinVar(t, &onDiskJournalMode, func(context.Context, SQLiteConn) (string, bool, error) { return "", false, nil })
	conn := &pragmaSpyConn{SQLiteConn: openWALTestConn(t, dbPath)}
	if mode, err := ApplyWALWithFallback(ctx, conn, "probe-unknown.db", false); err != nil || mode != "wal" {
		t.Fatalf("probe unknown = %q, %v", mode, err)
	}
	if got := conn.seen(); len(got) != 0 {
		t.Fatalf("pragmas issued = %v", got)
	}
	if journalModeOf(t, sibling) != "wal" {
		t.Fatal("sibling left WAL")
	}
	if !strings.Contains(buf.String(), "could not verify the on-disk journal mode") {
		t.Fatalf("no probe unknown warning in %s", buf.String())
	}
	_, err := ApplyWALWithFallback(ctx, conn, "probe-unknown.db", true)
	var unsupported *WALUnsupportedError
	if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "could not verify the on-disk journal mode") {
		t.Fatalf("require WAL probe unknown = %v", err)
	}
	if got := conn.seen(); len(got) != 0 {
		t.Fatalf("pragmas issued under require WAL = %v", got)
	}
}

func TestResolveJournalModeUsesRealDatabaseConfig(t *testing.T) {
	configureJournalMode(t, "DELETE")
	if got := ResolveJournalMode(context.Background()); got != "delete" {
		t.Fatalf("ResolveJournalMode = %q", got)
	}
}

func TestNewNonsecretEnvOverrideIsNotExposed(t *testing.T) {
	configureJournalMode(t, "wal")
	t.Setenv("ICEBATH_JOURNAL_MODE", "delete")
	if got := ResolveJournalMode(context.Background()); got != "wal" {
		t.Fatalf("ResolveJournalMode = %q", got)
	}
}

func TestInvalidConfigValueFallsBackToWAL(t *testing.T) {
	for _, value := range []string{"bogus", "truncate", "null", "42", "{bad: shape}"} {
		configureJournalMode(t, value)
		if got := ResolveJournalMode(context.Background()); got != "wal" {
			t.Fatalf("journal_mode %s = %q", value, got)
		}
	}
}

func TestMalformedDatabaseSectionFallsBackToWAL(t *testing.T) {
	for _, database := range []string{"[]", "delete", "42", "null"} {
		writeHomeConfig(t, "database: "+database+"\n")
		if got := ResolveJournalMode(context.Background()); got != "wal" {
			t.Fatalf("database %s = %q", database, got)
		}
	}
}

func TestApplyWALWithFallbackHonorsDeleteConfig(t *testing.T) {
	resetWALOnceState(t)
	configureJournalMode(t, "delete")
	pinVar(t, &walResetVulnerable, func() bool { return false })
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "configured.db"))
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "configured.db", false); err != nil || mode != "delete" {
		t.Fatalf("configured delete = %q, %v", mode, err)
	}
	if journalModeOf(t, conn) != "delete" {
		t.Fatal("connection not in delete mode")
	}
}

func TestApplyWALWithFallbackDefaultsToWAL(t *testing.T) {
	resetWALOnceState(t)
	configureJournalMode(t, "wal")
	pinVar(t, &walResetVulnerable, func() bool { return false })
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "default.db"))
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "default.db", false); err != nil || mode != "wal" {
		t.Fatalf("default = %q, %v", mode, err)
	}
	if journalModeOf(t, conn) != "wal" {
		t.Fatal("connection not in WAL mode")
	}
}

func TestConfiguredDeleteValidatesVulnerableSQLiteResult(t *testing.T) {
	resetWALOnceState(t)
	configureJournalMode(t, "delete")
	pinVar(t, &walResetVulnerable, func() bool { return true })
	conn := openWALTestConn(t, ":memory:")
	_, err := ApplyWALWithFallback(context.Background(), conn, "memory-configured.db", false)
	if !isOperationalError(err) || !strings.Contains(err.Error(), "could not set configured journal_mode=delete (got memory)") {
		t.Fatalf("memory configured delete = %v", err)
	}
	if journalModeOf(t, conn) != "memory" {
		t.Fatal("in memory journal mode changed")
	}
}

func TestConfiguredDeleteNeverLiveDowngradesExistingWAL(t *testing.T) {
	resetWALOnceState(t)
	configureJournalMode(t, "delete")
	buf := captureSlog(t)
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "existing-wal.db"))
	if row, err := fetchOne(context.Background(), conn, "PRAGMA journal_mode=WAL"); err != nil || modeFromRow(row) != "wal" {
		t.Fatalf("seed WAL = %v, %v", row, err)
	}
	pinVar(t, &walResetVulnerable, func() bool { return true })
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "existing-wal.db", false); err != nil || mode != "wal" {
		t.Fatalf("existing WAL = %q, %v", mode, err)
	}
	if journalModeOf(t, conn) != "wal" {
		t.Fatal("existing WAL downgraded")
	}
	found := false
	for _, line := range logLinesWith(buf, "ERROR", "database.journal_mode=delete is configured") {
		found = found || strings.Contains(line, "on-disk")
	}
	if !found {
		t.Fatalf("expected a warning that configured delete was overridden by on-disk WAL: %s", buf.String())
	}
}

func TestConfiguredDeleteOverriddenWarnsOnNonVulnerableRuntimeToo(t *testing.T) {
	resetWALOnceState(t)
	configureJournalMode(t, "delete")
	pinVar(t, &walResetVulnerable, func() bool { return false })
	buf := captureSlog(t)
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "existing-wal.db"))
	if row, err := fetchOne(context.Background(), conn, "PRAGMA journal_mode=WAL"); err != nil || modeFromRow(row) != "wal" {
		t.Fatalf("seed WAL = %v, %v", row, err)
	}
	for range 2 {
		if mode, err := ApplyWALWithFallback(context.Background(), conn, "existing-wal.db", false); err != nil || mode != "wal" {
			t.Fatalf("existing WAL = %q, %v", mode, err)
		}
	}
	if journalModeOf(t, conn) != "wal" {
		t.Fatal("existing WAL downgraded")
	}
	if got := logLinesWith(buf, "ERROR", "database.journal_mode=delete is configured"); len(got) != 1 {
		t.Fatalf("override warnings = %d", len(got))
	}
}

func TestConfiguredDeleteOverriddenWarningFiresOncePerDB(t *testing.T) {
	resetWALOnceState(t)
	configureJournalMode(t, "delete")
	pinVar(t, &walResetVulnerable, func() bool { return true })
	buf := captureSlog(t)
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "existing-wal.db"))
	if _, err := fetchOne(context.Background(), conn, "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if mode, err := ApplyWALWithFallback(context.Background(), conn, "once.db", false); err != nil || mode != "wal" {
			t.Fatalf("existing WAL = %q, %v", mode, err)
		}
	}
	if got := logLinesWith(buf, "ERROR", "database.journal_mode=delete is configured"); len(got) != 1 {
		t.Fatalf("override warnings = %d", len(got))
	}
}

func TestConfiguredDeleteWithRequireWALAndExistingWALReturnsWAL(t *testing.T) {
	resetWALOnceState(t)
	configureJournalMode(t, "delete")
	pinVar(t, &walResetVulnerable, func() bool { return false })
	buf := captureSlog(t)
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "existing-wal.db"))
	if row, err := fetchOne(context.Background(), conn, "PRAGMA journal_mode=WAL"); err != nil || modeFromRow(row) != "wal" {
		t.Fatalf("seed WAL = %v, %v", row, err)
	}
	if mode, err := ApplyWALWithFallback(context.Background(), conn, "existing-wal.db", true); err != nil || mode != "wal" {
		t.Fatalf("require WAL existing WAL = %q, %v", mode, err)
	}
	if got := logLinesWith(buf, "ERROR", "database.journal_mode=delete is configured"); len(got) == 0 {
		t.Fatal("no override warning")
	}
}
