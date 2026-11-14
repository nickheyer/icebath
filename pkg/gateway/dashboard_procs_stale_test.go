package gateway

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One row of ps output with a right aligned pid
func psLine(pid int, cmd string) string { return fmt.Sprintf("%7d %s", pid, cmd) }

// Probe double that answers ps with the given table and every other command with an empty success, recording all calls
func psRunner(t *testing.T, stdout string) *[][]string {
	t.Helper()
	calls := &[][]string{}
	swapSeam(t, &dashboardRunProbe, func(_ context.Context, argv []string, _ time.Duration) (*pysubprocess.Completed, error) {
		*calls = append(*calls, slices.Clone(argv))
		if len(argv) > 0 && argv[0] == "ps" {
			return &pysubprocess.Completed{Stdout: []byte(stdout)}, nil
		}
		return &pysubprocess.Completed{}, nil
	})
	return calls
}

// Writes a valid Desktop SSH backend lock under a scratch home and returns the pid it claims
func writeValidSSHBackendLock(t *testing.T) int {
	t.Helper()
	tmp := t.TempDir()
	oid, nonce := strings.Repeat("a", 32), strings.Repeat("b", 16)
	lockDir := filepath.Join(tmp, "desktop-ssh", oid)
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"schemaVersion": 2, "protocolVersion": 1, "ownershipId": %q, "spawnNonce": %q, "tokenFingerprint": %q, "pid": 4242, "port": 46369, `+
		`"profile": "default", "hermesPath": "/opt/icebath/bin/icebath", "hermesHome": %q, "logPath": "%s/desktop-ssh/%s/%s.log", "startedAt": "2026-08-21T15:27:39Z"}`,
		oid, nonce, strings.Repeat("c", 32), tmp, tmp, oid, nonce)
	if err := os.WriteFile(filepath.Join(lockDir, tools.BackendLockName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", tmp)
	t.Setenv("ICEBATH_DESKTOP_CHILD_PID", "")
	return 4242
}

func emptyKillResult(r DashboardKillResult) bool {
	return len(r.Matched) == 0 && len(r.Killed) == 0 && len(r.Failed) == 0 && r.Unrecovered == nil
}

func TestUpdateCleanupSparesBackendOwnedByValidSshLock(t *testing.T) {
	pid := writeValidSSHBackendLock(t)
	swapSeam(t, &dashboardFindStale, func(_ context.Context, exclude map[int]bool, _ string) []int {
		if !exclude[pid] {
			t.Errorf("exclude = %v", exclude)
		}
		return []int{}
	})
	calls := psRunner(t, "")
	result := KillStaleDashboardProcesses(context.Background(), &bytes.Buffer{}, DefaultDashboardKillReason, true, nil, "")
	if !emptyKillResult(result) {
		t.Fatalf("result = %+v", result)
	}
	for _, call := range *calls {
		if call[0] != "systemctl" || !slices.Contains(call, "list-unit-files") {
			t.Fatalf("unexpected call %v", call)
		}
	}
	if (len(*calls) > 0) != (runtime.GOOS != "windows") {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestExplicitStopDoesNotSpareBackendOwnedByValidSshLock(t *testing.T) {
	pid := writeValidSSHBackendLock(t)
	swapSeam(t, &dashboardFindStale, func(_ context.Context, exclude map[int]bool, _ string) []int {
		if exclude[pid] {
			t.Errorf("exclude = %v", exclude)
		}
		return []int{}
	})
	result := KillStaleDashboardProcesses(context.Background(), &bytes.Buffer{}, DefaultDashboardKillReason, false, nil, "")
	if !emptyKillResult(result) {
		t.Fatalf("result = %+v", result)
	}
}

func TestFindStaleDashboardPidsSelfPidExcluded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ps-based scan path")
	}
	pinServeGrammar(t)
	swapSeam(t, &dashboardLedgerEntries, func() []map[string]any { return nil })
	psRunner(t, psLine(os.Getpid(), "python3 -m icebath_cli.main dashboard")+"\n"+psLine(12345, "icebath dashboard --port 9119")+"\n")
	pids := findStaleDashboardPIDs(context.Background(), nil, "")
	if slices.Contains(pids, os.Getpid()) || !slices.Contains(pids, 12345) {
		t.Fatalf("pids = %v", pids)
	}
}

// A ps scan that times out yields no pids
func assertPsTimeoutReturnsEmpty(t *testing.T) {
	t.Helper()
	pinServeGrammar(t)
	swapSeam(t, &dashboardLedgerEntries, func() []map[string]any { return nil })
	swapSeam(t, &dashboardRunProbe, func(_ context.Context, argv []string, timeout time.Duration) (*pysubprocess.Completed, error) {
		return nil, &pysubprocess.TimeoutExpired{}
	})
	if pids := findStaleDashboardPIDs(context.Background(), nil, ""); len(pids) != 0 {
		t.Fatalf("pids = %v", pids)
	}
}

func TestFindStaleDashboardPidsPsTimeoutReturnsEmptyLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	assertPsTimeoutReturnsEmpty(t)
}

func TestFindStaleDashboardPidsPsTimeoutReturnsEmptyMacos(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macos only")
	}
	assertPsTimeoutReturnsEmpty(t)
}

func TestKillStaleDashboardPosixSigtermGracefulExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX kill semantics")
	}
	swapSeam(t, &dashboardGOOS, "linux")
	swapSeam(t, &dashboardFindStale, func(context.Context, map[int]bool, string) []int { return []int{12345, 12346} })
	psRunner(t, "")
	var signals [][2]int
	swapSeam(t, &dashboardKillPid, func(pid int, force bool) error {
		sig := 15
		if force {
			sig = 9
		}
		signals = append(signals, [2]int{pid, sig})
		return nil
	})
	swapSeam(t, &dashboardPidExists, func(int) bool { return false })
	swapSeam(t, &dashboardSleep, func(time.Duration) {})
	result := KillStaleDashboardProcesses(context.Background(), &bytes.Buffer{}, DefaultDashboardKillReason, false, nil, "")
	sigterms := []int{}
	for _, s := range signals {
		if s[1] == 9 {
			t.Fatalf("SIGKILL was sent: %v", signals)
		}
		sigterms = append(sigterms, s[0])
	}
	slices.Sort(sigterms)
	if !slices.Equal(sigterms, []int{12345, 12346}) {
		t.Fatalf("sigterms = %v", sigterms)
	}
	if !slices.Equal(result.Matched, []int{12345, 12346}) || !slices.Equal(result.Killed, []int{12345, 12346}) || len(result.Failed) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestKillStaleDashboardPosixUserScopeRestartNeverFallsBackToSystemOrSudo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX kill semantics")
	}
	swapSeam(t, &dashboardGOOS, "linux")
	unit := dashboardSystemdUnit
	var calls [][]string
	swapSeam(t, &dashboardRunProbe, func(_ context.Context, argv []string, _ time.Duration) (*pysubprocess.Completed, error) {
		calls = append(calls, slices.Clone(argv))
		switch strings.Join(argv, " ") {
		case "systemctl --user list-unit-files " + unit + " --no-legend --no-pager":
			return &pysubprocess.Completed{Stdout: []byte(unit + " enabled enabled\n")}, nil
		case "systemctl --user is-active " + unit:
			return &pysubprocess.Completed{Stdout: []byte("active\n")}, nil
		case "systemctl --user is-enabled " + unit:
			return &pysubprocess.Completed{Stdout: []byte("enabled\n")}, nil
		case "systemctl --user restart " + unit:
			return &pysubprocess.Completed{}, nil
		}
		t.Errorf("unexpected probe call: %v", argv)
		return &pysubprocess.Completed{}, nil
	})
	swapSeam(t, &dashboardFindStale, func(context.Context, map[int]bool, string) []int { return []int{} })
	swapSeam(t, &dashboardLockOwnedServePIDs, func() map[int]bool { return map[int]bool{} })
	killed := 0
	swapSeam(t, &dashboardKillPid, func(int, bool) error { killed++; return nil })
	KillStaleDashboardProcesses(context.Background(), &bytes.Buffer{}, DefaultDashboardKillReason, true, nil, "")
	if !slices.ContainsFunc(calls, func(c []string) bool { return slices.Equal(c, []string{"systemctl", "--user", "restart", unit}) }) {
		t.Fatalf("calls = %v", calls)
	}
	for _, call := range calls {
		if call[0] == "sudo" || len(call) < 2 || call[0] != "systemctl" || call[1] != "--user" {
			t.Fatalf("call left the user scope: %v", call)
		}
	}
	if killed != 0 {
		t.Fatalf("kill was called %d times", killed)
	}
}

func TestKillStaleDashboardWindowsTaskkillInvokedForEachPid(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	swapSeam(t, &dashboardFindStale, func(context.Context, map[int]bool, string) []int { return []int{12345, 12346} })
	swapSeam(t, &dashboardStartTime, func(int) any { return 123 })
	swapSeam(t, &dashboardProcCmdline, func(int) ([]string, error) { return []string{`C:\icebath\icebath.exe`, "dashboard"}, nil })
	swapSeam(t, &dashboardProcExe, func(int) (string, error) { return `C:\icebath\icebath.exe`, nil })
	calls := &[][]string{}
	swapSeam(t, &dashboardRunHidden, func(_ context.Context, argv []string, _ time.Duration) (*pysubprocess.Completed, error) {
		*calls = append(*calls, slices.Clone(argv))
		return &pysubprocess.Completed{}, nil
	})
	KillStaleDashboardProcesses(context.Background(), &bytes.Buffer{}, DefaultDashboardKillReason, false, nil, "")
	taskkills := [][]string{}
	for _, call := range *calls {
		if call[0] == "taskkill" {
			taskkills = append(taskkills, call)
		}
	}
	has := func(want ...string) bool {
		return slices.ContainsFunc(taskkills, func(c []string) bool { return slices.Equal(c, want) })
	}
	if len(taskkills) != 2 || !has("taskkill", "/PID", "12345", "/F") || !has("taskkill", "/PID", "12346", "/F") {
		t.Fatalf("taskkill calls = %v", taskkills)
	}
}
