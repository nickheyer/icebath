//go:build !windows

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Version tuple of three small parts
func gateVersion(parts [3]int64) state.VersionTuple {
	return state.NewVersionTuple(parts[0], parts[1], parts[2])
}

func TestWalResetVersionGateVulnerableVersions(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	for _, v := range [][3]int64{{3, 45, 1}, {3, 46, 1}, {3, 44, 5}, {3, 50, 4}, {3, 51, 2}, {3, 8, 0}} {
		if !state.IsSQLiteWALResetVulnerable(gateVersion(v)) {
			t.Fatalf("%v reported fixed", v)
		}
	}
}

func TestWalResetVersionGateFixedVersions(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	for _, v := range [][3]int64{{3, 44, 6}, {3, 44, 7}, {3, 50, 7}, {3, 50, 8}, {3, 51, 3}, {3, 51, 4}, {3, 52, 0}, {3, 53, 1}, {4, 0, 0}} {
		if state.IsSQLiteWALResetVulnerable(gateVersion(v)) {
			t.Fatalf("%v reported vulnerable", v)
		}
	}
}

func TestWalResetVersionGateGateMirrorsLibraryGate(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	for _, v := range [][3]int64{{3, 44, 5}, {3, 44, 6}, {3, 45, 1}, {3, 50, 4}, {3, 50, 7}, {3, 51, 2}, {3, 51, 3}, {3, 53, 1}} {
		info := state.SQLiteRuntimeInfo{SQLiteVersion: gateVersion(v)}
		if state.IsSQLiteWALResetVulnerable(gateVersion(v)) != info.WALResetVulnerable() {
			t.Fatalf("salvage gate disagrees with the runtime gate at %v", v)
		}
	}
}

// Directory holding a sqlite3 shell script with the given version line and recover exit status, put alone on PATH
func gateFakeCLI(t *testing.T, versionLine string, recoverStatus int) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n  printf '%s\\n' '" + versionLine + "'\n  exit 0\nfi\nexit " +
		string(rune('0'+recoverStatus)) + "\n"
	path := filepath.Join(dir, "sqlite3")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return path
}

func TestFindSqlite3CliRefusalMissingBinaryRefusal(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	t.Setenv("PATH", t.TempDir())
	if path, ok := findSQLite3CLI(); ok {
		t.Fatalf("found %q", path)
	}
	if got := findSQLite3CLIRefusal().GetOr("reason", nil); got != "missing" {
		t.Fatalf("reason %v", got)
	}
}

func TestFindSqlite3CliRefusalNoDbpageRefusal(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	gateFakeCLI(t, "3.51.3 2026-01-01 00:00:00", 1)
	if path, ok := findSQLite3CLI(); ok {
		t.Fatalf("found %q", path)
	}
	if got := findSQLite3CLIRefusal().GetOr("reason", nil); got != "no_dbpage" {
		t.Fatalf("reason %v", got)
	}
}

func TestFindSqlite3CliRefusalWalResetVulnerableRefusal(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	gateFakeCLI(t, "3.45.1 2024-01-30 16:01:20", 0)
	if path, ok := findSQLite3CLI(); ok {
		t.Fatalf("found %q", path)
	}
	refusal := findSQLite3CLIRefusal()
	if refusal.GetOr("reason", nil) != "wal_reset_vulnerable" || refusal.GetOr("version", nil) != "3.45.1" {
		t.Fatalf("refusal %v", refusal)
	}
	if detail, _ := refusal.GetOr("detail", "").(string); !strings.Contains(detail, "WAL-reset") {
		t.Fatalf("detail %q", detail)
	}
}

func TestFindSqlite3CliRefusalFixedCapableCliAccepted(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	fake := gateFakeCLI(t, "3.51.3 2026-01-01 00:00:00", 0)
	path, ok := findSQLite3CLI()
	if !ok || path != fake {
		t.Fatalf("found %q %v", path, ok)
	}
	if refusal := findSQLite3CLIRefusal(); refusal.Len() != 0 {
		t.Fatalf("refusal %v", refusal)
	}
}

func TestFindSqlite3CliRefusalUnparsableVersionStillUsable(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	fake := gateFakeCLI(t, "no version here", 0)
	if path, ok := findSQLite3CLI(); !ok || path != fake {
		t.Fatalf("found %q %v", path, ok)
	}
}

func TestParseSqlite3CliVersionParsesModernOutput(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	fake := gateFakeCLI(t, "3.51.4 2026-XX-XX 12:34:56", 0)
	version, ok := lfParseSQLite3CLIVersion(fake)
	if !ok || version.Cmp(state.NewVersionTuple(3, 51, 4)) != 0 {
		t.Fatalf("version %v %v", version, ok)
	}
}

func TestParseSqlite3CliVersionUnexecutableReturnsNone(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	if version, ok := lfParseSQLite3CLIVersion(filepath.Join(t.TempDir(), "x")); ok {
		t.Fatalf("version %v", version)
	}
}

// Namespace icebath main builds for sessions recover
func gateRecoverNamespace(source string, overrides map[string]any) map[string]any {
	ns := map[string]any{"sessions_action": "recover", "source": source, "output": nil, "inspect_only": false,
		"work_dir": nil, "chunk_size": 1000, "allow_partial": false, "report": nil}
	for k, v := range overrides {
		ns[k] = v
	}
	return ns
}

// Database holding one plain table
func gateTableDB(t *testing.T, path string) {
	t.Helper()
	conn := recoveryTestConn(t, path)
	recoveryTestExec(t, conn, "CREATE TABLE t (x)")
}

func TestEmittedCommandsSatisfyCliContractOldV1ShapeIsStillRejected(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	h := tuiTestHome(t)
	if code, _ := runSessionsCmd(t, h, gateRecoverNamespace(filepath.Join(t.TempDir(), "state.db"), nil), ""); code != 2 {
		t.Fatalf("rc %d", code)
	}
}

func TestEmittedCommandsSatisfyCliContractInspectStageDispatchesPastGate(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	h := tuiTestHome(t)
	source := filepath.Join(t.TempDir(), "state.db")
	gateTableDB(t, source)
	if code, out := runSessionsCmd(t, h, gateRecoverNamespace(source, map[string]any{"inspect_only": true}), ""); code == 2 {
		t.Fatalf("--inspect-only shape must pass the contract gate: %s", out)
	}
}

func TestEmittedCommandsSatisfyCliContractOutputStageDispatchesPastGate(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	h := tuiTestHome(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "state.db")
	gateTableDB(t, source)
	if code, out := runSessionsCmd(t, h, gateRecoverNamespace(source, map[string]any{"output": filepath.Join(dir, "recovered-state.db")}), ""); code == 2 {
		t.Fatalf("--output shape must pass the contract gate: %s", out)
	}
}
