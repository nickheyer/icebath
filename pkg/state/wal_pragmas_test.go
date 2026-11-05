package state

import (
	"context"
	"errors"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
)

// Repair hint with the install method and its update command pinned
func hintFor(t *testing.T, method string) string {
	t.Helper()
	pinVar(t, &detectInstallMethod, func(string, string) (string, error) { return method, nil })
	pinVar(t, &recommendedUpdateCommandForMethod, func(m string) string { return "CMD-" + m })
	return walResetRepairHint(context.Background())
}

func TestWALResetRepairHintPerInstallMethod(t *testing.T) {
	cases := map[string]string{
		"source": "CMD-source",
		"git":    "icebath-managed installs can repair the embedded runtime with `CMD-git`",
		"docker": "update the container image with `CMD-docker`",
		"nix":    "CMD-nix",
	}
	for method, want := range cases {
		if got := hintFor(t, method); got != want {
			t.Errorf("hint for %s = %q want %q", method, got, want)
		}
	}
}

func TestHintFallsBackWhenDetectionFails(t *testing.T) {
	pinVar(t, &detectInstallMethod, func(string, string) (string, error) { return "", errors.New("boom") })
	if hint := walResetRepairHint(context.Background()); !strings.Contains(hint, "SQLite 3.51.3+") {
		t.Fatalf("fallback hint = %q", hint)
	}
}

func TestResolveSynchronousLevelAcceptsDocumentedSpellings(t *testing.T) {
	cases := []struct {
		raw  any
		want int
	}{
		{"OFF", 0}, {"NORMAL", 1}, {"FULL", 2}, {"EXTRA", 3}, {"full", 2}, {"Full", 2}, {"  FULL  ", 2},
		{0, 0}, {1, 1}, {2, 2}, {3, 3}, {"2", 2}, {" 2 ", 2}, {"o\ufb00", 0}, {int64(3), 3}, {big.NewInt(1), 1},
	}
	for _, c := range cases {
		if got, ok := ResolveSynchronousLevel(c.raw); !ok || got != c.want {
			t.Errorf("ResolveSynchronousLevel(%#v) = %d, %v want %d", c.raw, got, ok, c.want)
		}
	}
}

func TestResolveSynchronousLevelRejectsEverythingElse(t *testing.T) {
	for _, raw := range []any{"PARANOID", "", "   ", 4, -1, "4", nil, []any{}, map[string]any{}, 2.5, "2.0", true} {
		if got, ok := ResolveSynchronousLevel(raw); ok {
			t.Errorf("ResolveSynchronousLevel(%#v) = %d accepted", raw, got)
		}
	}
	if got, ok := ResolveSynchronousLevel(false); !ok || got != 0 {
		t.Fatalf("YAML off = %d, %v", got, ok)
	}
}

// Connection in WAL mode on a fresh state.db under the test directory
func walModeConn(t *testing.T) SQLiteConn {
	t.Helper()
	conn := openWALTestConn(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := fetchOne(context.Background(), conn, "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	return conn
}

// Integer a single value pragma reports on the connection
func pragmaInt(t *testing.T, conn SQLiteConn, pragma string) int64 {
	t.Helper()
	row, err := fetchOne(context.Background(), conn, "PRAGMA "+pragma)
	if err != nil {
		t.Fatal(err)
	}
	n, ok := row[0].(int64)
	if !ok {
		t.Fatalf("PRAGMA %s = %#v", pragma, row[0])
	}
	return n
}

// Runs a statement on the connection and fails the test on error
func mustExecConn(t *testing.T, conn SQLiteConn, query string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), query); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredSynchronousLevelReachesTheConnection(t *testing.T) {
	pinVar(t, &walPlatform, "linux")
	writeHomeConfig(t, "database:\n  synchronous: NORMAL\n")
	conn := walModeConn(t)
	if err := ApplyDatabasePragmas(context.Background(), conn, "state.db"); err != nil {
		t.Fatal(err)
	}
	if got := pragmaInt(t, conn, "synchronous"); got != 1 {
		t.Fatalf("synchronous = %d", got)
	}
}

func TestFullSynchronousReachesTheConnection(t *testing.T) {
	writeHomeConfig(t, "database:\n  synchronous: FULL\n")
	conn := walModeConn(t)
	mustExecConn(t, conn, "PRAGMA synchronous=0")
	if got := pragmaInt(t, conn, "synchronous"); got != 0 {
		t.Fatalf("synchronous before = %d", got)
	}
	if err := ApplyDatabasePragmas(context.Background(), conn, "state.db"); err != nil {
		t.Fatal(err)
	}
	if got := pragmaInt(t, conn, "synchronous"); got != 2 {
		t.Fatalf("synchronous = %d", got)
	}
}

func TestUnsetSynchronousLeavesTheLevelAlone(t *testing.T) {
	writeHomeConfig(t, "database:\n  wal_autocheckpoint: 1000\n")
	conn := walModeConn(t)
	mustExecConn(t, conn, "PRAGMA synchronous=0")
	if err := ApplyDatabasePragmas(context.Background(), conn, "state.db"); err != nil {
		t.Fatal(err)
	}
	if got := pragmaInt(t, conn, "synchronous"); got != 0 {
		t.Fatalf("synchronous = %d", got)
	}
	if got := pragmaInt(t, conn, "wal_autocheckpoint"); got != 1000 {
		t.Fatalf("wal_autocheckpoint = %d", got)
	}
}

func TestGarbageSynchronousWarnsAndChangesNothing(t *testing.T) {
	writeHomeConfig(t, "database:\n  synchronous: PARANOID\n  cache_size: lots\n")
	buf := captureSlog(t)
	conn := walModeConn(t)
	mustExecConn(t, conn, "PRAGMA synchronous=2")
	if err := ApplyDatabasePragmas(context.Background(), conn, "state.db"); err != nil {
		t.Fatal(err)
	}
	if got := pragmaInt(t, conn, "synchronous"); got != 2 {
		t.Fatalf("synchronous = %d", got)
	}
	if len(logLinesWith(buf, "WARN", "PARANOID")) != 1 || len(logLinesWith(buf, "WARN", "ignoring non-integer database.cache_size=")) != 1 {
		t.Fatalf("warnings = %s", buf.String())
	}
}

func TestTheOtherPragmasStillApply(t *testing.T) {
	writeHomeConfig(t, "database:\n  synchronous: FULL\n  wal_autocheckpoint: 250\n  mmap_size: 0\n  cache_size: ' -4000 '\n")
	conn := walModeConn(t)
	if err := ApplyDatabasePragmas(context.Background(), conn, "state.db"); err != nil {
		t.Fatal(err)
	}
	for pragma, want := range map[string]int64{"synchronous": 2, "wal_autocheckpoint": 250, "mmap_size": 0, "cache_size": -4000} {
		if got := pragmaInt(t, conn, pragma); got != want {
			t.Errorf("%s = %d want %d", pragma, got, want)
		}
	}
}

func TestLoweringBelowFullIsRefusedOnDarwin(t *testing.T) {
	pinVar(t, &walPlatform, "darwin")
	writeHomeConfig(t, "database:\n  synchronous: NORMAL\n")
	buf := captureSlog(t)
	conn := walModeConn(t)
	if err := enforceMacOSSynchronousFull(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	if got := pragmaInt(t, conn, "synchronous"); got != 2 {
		t.Fatalf("synchronous after floor = %d", got)
	}
	if err := ApplyDatabasePragmas(context.Background(), conn, "state.db"); err != nil {
		t.Fatal(err)
	}
	if got := pragmaInt(t, conn, "synchronous"); got != 2 {
		t.Fatal("config lowered the macOS btree protection")
	}
	if len(logLinesWith(buf, "WARN", "refusing database.synchronous=NORMAL on macOS")) != 1 {
		t.Fatalf("warnings = %s", buf.String())
	}
}

func TestRaisingAboveFullIsAllowedOnDarwin(t *testing.T) {
	pinVar(t, &walPlatform, "darwin")
	writeHomeConfig(t, "database:\n  synchronous: EXTRA\n")
	conn := walModeConn(t)
	if err := ApplyDatabasePragmas(context.Background(), conn, "state.db"); err != nil {
		t.Fatal(err)
	}
	if got := pragmaInt(t, conn, "synchronous"); got != 3 {
		t.Fatalf("synchronous = %d", got)
	}
}

func TestWALCompanionsApplySizeLimitAndDarwinBarriers(t *testing.T) {
	pinVar(t, &walPlatform, "darwin")
	conn := walModeConn(t)
	mustExecConn(t, conn, "PRAGMA synchronous=1")
	if err := applyWALCompanions(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	for pragma, want := range map[string]int64{"journal_size_limit": walSizeLimitBytes, "checkpoint_fullfsync": 1, "synchronous": 2} {
		if got := pragmaInt(t, conn, pragma); got != want {
			t.Errorf("%s = %d want %d", pragma, got, want)
		}
	}
	walPlatform = "linux"
	other := walModeConn(t)
	mustExecConn(t, other, "PRAGMA synchronous=1")
	if err := applyWALCompanions(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if pragmaInt(t, other, "checkpoint_fullfsync") != 0 || pragmaInt(t, other, "synchronous") != 1 {
		t.Fatal("darwin barriers applied off macOS")
	}
}
