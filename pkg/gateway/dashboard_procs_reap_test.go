package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Holder grammar of the tests, the token after an icebath launcher or the icebath main module
func pinServeGrammar(t *testing.T) {
	t.Helper()
	swapSeam(t, &HolderSubcommand, func(cmdline string) (string, bool) {
		fields := strings.Fields(cmdline)
		for i, f := range fields {
			if (filepath.Base(f) == "icebath" || f == "icebath_cli.main") && i+1 < len(fields) {
				return fields[i+1], true
			}
		}
		return "", false
	})
}

// Kill recorder over a set of live pids, SIGTERM and SIGKILL both removing the pid
type reapKills struct {
	live  map[int]bool
	terms []int
	sent  [][2]int
	deny  map[int]bool
}

func (k *reapKills) kill(pid int, force bool) error {
	if k.deny[pid] {
		return &os.SyscallError{Syscall: "kill", Err: syscall.EPERM}
	}
	sig := 15
	if force {
		sig = 9
	}
	k.sent = append(k.sent, [2]int{pid, sig})
	if !force {
		k.terms = append(k.terms, pid)
	}
	delete(k.live, pid)
	return nil
}

// Pins the scan, parent lookup and host facts of one reap run and returns the scan excludes
func reapSeams(t *testing.T, scanned []pidCommand, ppids map[int]int, kills *reapKills) *map[int]bool {
	t.Helper()
	pinServeGrammar(t)
	t.Setenv("ICEBATH_DESKTOP_CHILD_PID", "")
	swapSeam(t, &dashboardGOOS, "linux")
	seen := &map[int]bool{}
	swapSeam(t, &dashboardScan, func(_ context.Context, exclude map[int]bool) []pidCommand {
		*seen = maps.Clone(exclude)
		return scanned
	})
	swapSeam(t, &dashboardProcessPpid, func(_ context.Context, pid int) (int, bool) {
		if ppids == nil {
			return 1, true
		}
		ppid, ok := ppids[pid]
		return ppid, ok
	})
	swapSeam(t, &dashboardPosixDescendants, func(context.Context, []int) []dashboardDescendant { return nil })
	swapSeam(t, &dashboardLockOwnedServePIDs, func() map[int]bool { return map[int]bool{} })
	swapSeam(t, &dashboardPidExists, func(pid int) bool { return kills != nil && kills.live[pid] })
	return seen
}

func reapOptions(kills *reapKills, age func(int) (float64, error)) DashboardReapOptions {
	return DashboardReapOptions{Sleep: func(time.Duration) {}, Kill: kills.kill, ProcessAgeSeconds: age}
}

func oldEnough(int) (float64, error) { return 600.0, nil }

func sortedPIDs(pids []int) []int {
	out := slices.Clone(pids)
	slices.Sort(out)
	return out
}

func TestDesktopLocalServeShapeMatchesEphemeralLoopback(t *testing.T) {
	pinServeGrammar(t)
	for _, command := range []string{
		"python -m icebath_cli.main serve --host 127.0.0.1 --port 0",
		"icebath serve --isolated --host 127.0.0.1 --port 0 --ssh-owner-nonce abc",
		"/venv/bin/icebath serve --host=127.0.0.1 --port=0",
	} {
		if !isDesktopLocalServeCmdline(command) {
			t.Fatalf("%q was not matched", command)
		}
	}
}

func TestDesktopLocalServeShapeSparesFixedPortAndNonServe(t *testing.T) {
	pinServeGrammar(t)
	for _, command := range []string{
		"icebath serve --host 100.106.105.2 --port 9119 --skip-build",
		"icebath serve --host 127.0.0.1 --port 9119",
		"icebath gateway run --replace",
		"icebath kanban --preserve-cache --host 127.0.0.1 --port 0",
	} {
		if isDesktopLocalServeCmdline(command) {
			t.Fatalf("%q was matched", command)
		}
	}
}

func TestReapOnlyKillsPpid1LocalServes(t *testing.T) {
	scanned := []pidCommand{
		{111, "icebath serve --host 127.0.0.1 --port 0"},
		{222, "icebath serve --host 127.0.0.1 --port 0"},
		{333, "icebath serve --host 100.1.2.3 --port 9119"},
		{444, "icebath serve --isolated --host 127.0.0.1 --port 0"},
	}
	kills := &reapKills{live: map[int]bool{111: true, 222: true, 333: true, 444: true}}
	reapSeams(t, scanned, map[int]int{111: 1, 222: 50, 333: 1, 444: 1}, kills)
	result := ReapOrphanedDesktopLocalServes(context.Background(), &bytes.Buffer{}, reapOptions(kills, oldEnough))
	want := []int{111, 444}
	if !slices.Equal(sortedPIDs(result.Matched), want) || !slices.Equal(sortedPIDs(kills.terms), want) || !slices.Equal(sortedPIDs(result.Killed), want) {
		t.Fatalf("matched = %v terms = %v killed = %v", result.Matched, kills.terms, result.Killed)
	}
}

func TestReapPassesChildPidExcludeToScan(t *testing.T) {
	seen := reapSeams(t, nil, nil, nil)
	t.Setenv("ICEBATH_DESKTOP_CHILD_PID", "999,111")
	result := ReapOrphanedDesktopLocalServes(context.Background(), &bytes.Buffer{}, DashboardReapOptions{Sleep: func(time.Duration) {}})
	if len(result.Matched) != 0 {
		t.Fatalf("matched = %v", result.Matched)
	}
	if !(*seen)[111] || !(*seen)[999] {
		t.Fatalf("exclude = %v", *seen)
	}
}

func TestReapSpareLockOwnedSshRemoteBackendOfForeignClient(t *testing.T) {
	scanned := []pidCommand{{555, "icebath serve --host 127.0.0.1 --port 0"}, {666, "icebath serve --host 127.0.0.1 --port 0"}}
	kills := &reapKills{live: map[int]bool{555: true, 666: true}}
	reapSeams(t, scanned, map[int]int{555: 1, 666: 1}, kills)
	opts := reapOptions(kills, oldEnough)
	opts.LockOwnedPIDs = func() (map[int]bool, error) { return map[int]bool{555: true}, nil }
	result := ReapOrphanedDesktopLocalServes(context.Background(), &bytes.Buffer{}, opts)
	if !slices.Equal(result.Matched, []int{666}) || !slices.Equal(kills.terms, []int{666}) || !slices.Equal(result.Killed, []int{666}) {
		t.Fatalf("matched = %v terms = %v killed = %v", result.Matched, kills.terms, result.Killed)
	}
}

func TestReapSparesYoungBackendUntilDesktopCanWriteLock(t *testing.T) {
	kills := &reapKills{live: map[int]bool{777: true}}
	reapSeams(t, []pidCommand{{777, "icebath serve --isolated --host 127.0.0.1 --port 0"}}, nil, kills)
	result := ReapOrphanedDesktopLocalServes(context.Background(), &bytes.Buffer{}, reapOptions(kills, func(int) (float64, error) { return 2.0, nil }))
	if len(result.Matched) != 0 || len(result.Killed) != 0 || len(kills.terms) != 0 {
		t.Fatalf("matched = %v killed = %v terms = %v", result.Matched, result.Killed, kills.terms)
	}
}

func TestReapSparesBackendWhenProcessAgeIsUnknown(t *testing.T) {
	kills := &reapKills{live: map[int]bool{778: true}}
	reapSeams(t, []pidCommand{{778, "icebath serve --isolated --host 127.0.0.1 --port 0"}}, nil, kills)
	age := func(int) (float64, error) { return 0, errors.New("process disappeared during age probe") }
	result := ReapOrphanedDesktopLocalServes(context.Background(), &bytes.Buffer{}, reapOptions(kills, age))
	if len(result.Matched) != 0 || len(result.Killed) != 0 || len(kills.terms) != 0 {
		t.Fatalf("matched = %v killed = %v terms = %v", result.Matched, result.Killed, kills.terms)
	}
}

func TestReapAgeBoundaryMakes180SecondOrphanEligible(t *testing.T) {
	scanned := []pidCommand{{779, "icebath serve --isolated --host 127.0.0.1 --port 0"}, {780, "icebath serve --isolated --host 127.0.0.1 --port 0"}}
	kills := &reapKills{live: map[int]bool{779: true, 780: true}}
	reapSeams(t, scanned, nil, kills)
	ages := map[int]float64{779: 179.999, 780: 180.0}
	result := ReapOrphanedDesktopLocalServes(context.Background(), &bytes.Buffer{}, reapOptions(kills, func(pid int) (float64, error) { return ages[pid], nil }))
	if !slices.Equal(result.Matched, []int{780}) || !slices.Equal(result.Killed, []int{780}) || !slices.Equal(kills.terms, []int{780}) {
		t.Fatalf("matched = %v killed = %v terms = %v", result.Matched, result.Killed, kills.terms)
	}
}

func TestReapSpareLockOwnedBackendEvenWithoutExcludeMatch(t *testing.T) {
	oid, nonce := strings.Repeat("a", 32), strings.Repeat("b", 16)
	lockRoot := filepath.Join(t.TempDir(), "desktop-ssh")
	if err := os.MkdirAll(filepath.Join(lockRoot, oid), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"schemaVersion": 2, "protocolVersion": 1, "ownershipId": %q, "spawnNonce": %q, "tokenFingerprint": %q, "pid": 4242, "port": 0, `+
		`"profile": "default", "hermesPath": "/opt/icebath/bin/icebath", "hermesHome": "~/.icebath", "logPath": "~/.icebath/desktop-ssh/%s/%s.log", "startedAt": "2026-08-04T20:00:00Z"}`,
		oid, nonce, strings.Repeat("a", 32), oid, nonce)
	if err := os.WriteFile(filepath.Join(lockRoot, oid, tools.BackendLockName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	kills := &reapKills{live: map[int]bool{4242: true}}
	reapSeams(t, []pidCommand{{4242, "icebath serve --host 127.0.0.1 --port 0"}}, nil, kills)
	opts := DashboardReapOptions{Sleep: func(time.Duration) {}, Kill: kills.kill,
		LockOwnedPIDs: func() (map[int]bool, error) { return tools.LockOwnedServePIDs(lockRoot), nil }}
	result := ReapOrphanedDesktopLocalServes(context.Background(), &bytes.Buffer{}, opts)
	if len(kills.terms) != 0 || len(result.Matched) != 0 {
		t.Fatalf("terms = %v matched = %v", kills.terms, result.Matched)
	}
}

func TestReapKillsDescendantsOfKilledRootsButSparesAFailedRootsSubtree(t *testing.T) {
	scanned := []pidCommand{{111, "icebath serve --host 127.0.0.1 --port 0"}, {444, "icebath serve --host 127.0.0.1 --port 0"}}
	kills := &reapKills{live: map[int]bool{111: true, 444: true, 1111: true, 4444: true}, deny: map[int]bool{444: true}}
	reapSeams(t, scanned, nil, kills)
	swapSeam(t, &dashboardPidExists, func(int) bool { return true })
	var snapshots [][]int
	swapSeam(t, &dashboardPosixDescendants, func(_ context.Context, roots []int) []dashboardDescendant {
		snapshots = append(snapshots, slices.Clone(roots))
		return []dashboardDescendant{{1111, 111, 5.0}, {4444, 444, 6.0}}
	})
	startTimes := map[int]any{1111: 5.0, 4444: 6.0}
	swapSeam(t, &dashboardStartTime, func(pid int) any { return startTimes[pid] })
	result := ReapOrphanedDesktopLocalServes(context.Background(), &bytes.Buffer{}, reapOptions(kills, oldEnough))
	if len(snapshots) != 1 || !slices.Equal(snapshots[0], []int{111, 444}) {
		t.Fatalf("snapshots = %v", snapshots)
	}
	if !slices.Equal(result.Killed, []int{111}) || !slices.Equal(result.Failed, []int{444}) {
		t.Fatalf("killed = %v failed = %v", result.Killed, result.Failed)
	}
	if !slices.Contains(kills.sent, [2]int{1111, 9}) {
		t.Fatalf("surviving child of the killed backend was not swept: %v", kills.sent)
	}
	for _, s := range kills.sent {
		if s[0] == 4444 {
			t.Fatalf("child of a root we could not kill was swept: %v", kills.sent)
		}
	}
}

func TestDesktopLocalServeSparesFixedPortAndRemoteHosts(t *testing.T) {
	pinServeGrammar(t)
	if isDesktopLocalServeCmdline("icebath serve --host 100.106.105.2 --port 9119 --skip-build") {
		t.Fatal("a remote host serve was matched")
	}
	if isDesktopLocalServeCmdline("icebath serve --host 127.0.0.1 --port 9119") {
		t.Fatal("a fixed port serve was matched")
	}
	if !isDesktopLocalServeCmdline("icebath serve --host localhost --port 0") {
		t.Fatal("a localhost port zero serve was not matched")
	}
}
