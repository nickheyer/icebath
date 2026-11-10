package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Temp home owning its gateway pid like the owned_process_home fixture, the Task Scheduler probe inert like the module fixture
func ownedStopHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(tmp, "locks"))
	swapSetup(t, &homeForPIDHook, func(context.Context, int, bool) (string, bool, error) { return processHermesHome(), true, nil })
	swapSetup(t, &reapTaskState, func(context.Context, string) (string, bool) { return "", false })
	return h
}

// Host with no systemctl or powershell on PATH, the reaper's supports_systemd_services answering false
func unsupervisedHost(t *testing.T, goos string) {
	t.Helper()
	swapSetup(t, &serviceGOOS, goos)
	swapSetup(t, &setupSupportsSystemd, func(context.Context) bool { return false })
	swapSetup(t, &serviceLookPath, func(name string) (string, error) { return "", fmt.Errorf("%s: %w", name, os.ErrNotExist) })
}

// Running pid probe answering pid while alive reports true
func runningPid(t *testing.T, pid int, alive func() bool) {
	t.Helper()
	swapSetup(t, &statusGetRunningPid, func(string, bool) (int, bool, error) {
		if alive != nil && !alive() {
			return 0, false, nil
		}
		return pid, true, nil
	})
}

// Pid existence answering true for the first polls calls only
func exitsAfter(polls int) func(int) bool {
	seen := 0
	return func(int) bool {
		seen++
		return seen <= polls
	}
}

func noSleep(float64) {}

func TestASlowButHealthyShutdownIsNotForceKilled(t *testing.T) {
	if got := awaitGatewayExit([]int{4242}, exitsAfter(60), noSleep, orphanExitGraceSeconds, orphanExitPollSeconds); len(got) != 0 {
		t.Fatalf("survivors %v", got)
	}
}

func TestATrulyHungProcessIsStillReportedForForceKill(t *testing.T) {
	got := awaitGatewayExit([]int{4242}, func(int) bool { return true }, noSleep, orphanExitGraceSeconds, orphanExitPollSeconds)
	if !reflect.DeepEqual(got, []int{4242}) {
		t.Fatalf("survivors %v", got)
	}
}

func TestAnAlreadyDeadProcessReturnsImmediately(t *testing.T) {
	var slept []float64
	got := awaitGatewayExit([]int{4242}, func(int) bool { return false }, func(s float64) { slept = append(slept, s) },
		orphanExitGraceSeconds, orphanExitPollSeconds)
	if len(got) != 0 || len(slept) != 0 {
		t.Fatalf("survivors %v slept %v, waited on a process that was already gone", got, slept)
	}
}

func TestTheGracePeriodCoversALargeWalCheckpoint(t *testing.T) {
	if orphanExitGraceSeconds < 20.0 {
		t.Fatalf("grace %v", orphanExitGraceSeconds)
	}
}

func TestForceKillIsLoggedSoTheNextIncidentHasEvidence(t *testing.T) {
	logs := captureLogs(t)
	var killed []int
	forceKillSurvivors([]int{4242}, func(pid int) error { killed = append(killed, pid); return nil })
	if !reflect.DeepEqual(killed, []int{4242}) {
		t.Fatalf("killed %v", killed)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "4242") {
		t.Fatalf("a force-kill left no trace in the log: %q", logs.String())
	}
}

func TestAPidThatVanishesInTheFinalIntervalIsNotKilled(t *testing.T) {
	polls := int(orphanExitGraceSeconds / 0.2)
	if got := awaitGatewayExit([]int{4242}, exitsAfter(polls), noSleep, orphanExitGraceSeconds, orphanExitPollSeconds); len(got) != 0 {
		t.Fatalf("a process that exited during the last interval was killed: %v", got)
	}
}

// Seams of the Windows stop cases, recording drain, force and kill calls in order
func windowsStopCalls(t *testing.T, drained bool) *[]string {
	t.Helper()
	var calls []string
	swapSetup(t, &serviceGOOS, "windows")
	swapSetup(t, &stopDrainTimeout, func(context.Context) float64 { return 7.0 })
	swapSetup(t, &stopDrainPid, func(_ context.Context, pid int, timeout float64) bool {
		calls = append(calls, fmt.Sprintf("drain %d %v", pid, timeout))
		return drained
	})
	swapSetup(t, &stopForceTerminate, func(ids []gatewayPidIdentity) int {
		calls = append(calls, fmt.Sprintf("force %v", ids))
		return 0
	})
	swapSetup(t, &stopKill, func(int, bool) error { calls = append(calls, "kill"); return nil })
	swapSetup(t, &statusPidExists, func(int) bool { return false })
	swapSetup(t, &stopRemovePidFile, func() {})
	swapSetup(t, &stopReapOrphans, func(context.Context, map[int]bool, float64) (bool, error) { return false, nil })
	return &calls
}

func TestStopProfileGatewayWindowsStopDrainsMarkerBeforeForceTermination(t *testing.T) {
	ownedStopHome(t)
	runningPid(t, 12345, nil)
	calls := windowsStopCalls(t, true)
	if ok, err := StopProfileGateway(context.Background()); err != nil || !ok {
		t.Fatalf("stop %v %v", ok, err)
	}
	if !reflect.DeepEqual(*calls, []string{"drain 12345 7"}) {
		t.Fatalf("calls %v", *calls)
	}
}

func TestStopProfileGatewayWindowsStopForceTerminatesOnlyAfterDrainTimeout(t *testing.T) {
	ownedStopHome(t)
	runningPid(t, 12345, nil)
	calls := windowsStopCalls(t, false)
	swapSetup(t, &statusProcessStartTime, func(int) any { return 100 })
	if ok, err := StopProfileGateway(context.Background()); err != nil || !ok {
		t.Fatalf("stop %v %v", ok, err)
	}
	if !reflect.DeepEqual(*calls, []string{"drain 12345 7", "force [{12345 100}]"}) {
		t.Fatalf("calls %v", *calls)
	}
}

func TestStopProfileGatewayWindowsStopForceKillCarriesPreDrainIdentity(t *testing.T) {
	ownedStopHome(t)
	pid, now, alive := 4242, 0.0, true
	swapSetup(t, &serviceGOOS, "windows")
	swapSetup(t, &winMonotonic, func() float64 { return now })
	swapSetup(t, &winSleep, func(time.Duration) { now += 10.0 })
	swapSetup(t, &stopDrainTimeout, func(context.Context) float64 { return 7.0 })
	runningPid(t, pid, func() bool { return alive })
	swapSetup(t, &lifecycleWritePlannedStop, func(context.Context, int) bool { return true })
	swapSetup(t, &statusPidExists, func(int) bool { return alive })
	swapSetup(t, &statusProcessStartTime, func(int) any {
		if now < 7.0 {
			return 100
		}
		return 999
	})
	var calls []string
	swapSetup(t, &statusTerminatePid, func(target int, force bool, expected any) error {
		calls = append(calls, fmt.Sprintf("%d %v %v", target, force, expected))
		alive = false
		return nil
	})
	swapSetup(t, &stopRemovePidFile, func() {})
	swapSetup(t, &stopReapOrphans, func(context.Context, map[int]bool, float64) (bool, error) { return false, nil })
	if ok, err := StopProfileGateway(context.Background()); err != nil || !ok {
		t.Fatalf("stop %v %v", ok, err)
	}
	if !reflect.DeepEqual(calls, []string{"4242 true 100"}) {
		t.Fatalf("calls %v", calls)
	}
}

func TestStopProfileGatewayKeepsPidFileWhenProcessStillRunning(t *testing.T) {
	ownedStopHome(t)
	swapSetup(t, &serviceGOOS, "linux")
	runningPid(t, 12345, nil)
	kills, probes, removes, reaps := 0, 0, 0, 0
	swapSetup(t, &stopKill, func(int, bool) error { kills++; return nil })
	swapSetup(t, &statusPidExists, func(int) bool { probes++; return true })
	swapSetup(t, &stopSleep, func(time.Duration) {})
	swapSetup(t, &stopRemovePidFile, func() { removes++ })
	swapSetup(t, &stopReapOrphans, func(context.Context, map[int]bool, float64) (bool, error) { reaps++; return false, nil })
	if ok, err := StopProfileGateway(context.Background()); err != nil || !ok {
		t.Fatalf("stop %v %v", ok, err)
	}
	if kills != 1 || removes != 0 || reaps != 1 {
		t.Fatalf("kills %d removes %d reaps %d probes %d", kills, removes, reaps, probes)
	}
}

func TestStopProfileGatewayExcludesKilledPidFromOrphanReap(t *testing.T) {
	ownedStopHome(t)
	swapSetup(t, &serviceGOOS, "linux")
	runningPid(t, 99999, nil)
	swapSetup(t, &stopKill, func(int, bool) error { return nil })
	swapSetup(t, &statusPidExists, func(int) bool { return false })
	swapSetup(t, &stopSleep, func(time.Duration) {})
	swapSetup(t, &stopRemovePidFile, func() {})
	var excludes []map[int]bool
	swapSetup(t, &stopReapOrphans, func(_ context.Context, extra map[int]bool, _ float64) (bool, error) {
		if len(extra) > 0 {
			excludes = append(excludes, extra)
		}
		return false, nil
	})
	if ok, err := StopProfileGateway(context.Background()); err != nil || !ok {
		t.Fatalf("stop %v %v", ok, err)
	}
	if len(excludes) != 1 || !excludes[0][99999] {
		t.Fatalf("excludes %v", excludes)
	}
}

// Gateway scan returning the candidates the exclusion set does not cover
func scanReturns(t *testing.T, candidates ...int) {
	t.Helper()
	swapSetup(t, &reapFindGatewayPIDs, func(_ context.Context, exclude map[int]bool, _ bool) ([]int, error) {
		var out []int
		for _, p := range candidates {
			if !exclude[p] {
				out = append(out, p)
			}
		}
		return out, nil
	})
}

// Parent chain of a fake psutil, an unknown pid failing like its KeyError
func fakeParents(t *testing.T, chain map[int]int) {
	t.Helper()
	swapSetup(t, &reapParent, func(pid int) (int, bool, error) {
		parent, ok := chain[pid]
		if !ok {
			return 0, false, fmt.Errorf("KeyError: %d", pid)
		}
		return parent, parent != 0, nil
	})
}

func TestReapUnsupervisedGatewayOrphansMacOSMacosExcludesLaunchdPidFromKill(t *testing.T) {
	ownedStopHome(t)
	unsupervisedHost(t, "darwin")
	launchd, orphan := 52615, 99998
	swapSetup(t, &servicePIDsHook, func(context.Context, bool) ([]int, error) { return []int{launchd}, nil })
	swapSetup(t, &statusGetRunningPid, func(string, bool) (int, bool, error) { return 0, false, nil })
	scanReturns(t, launchd, orphan)
	var killed []int
	swapSetup(t, &stopKill, func(pid int, _ bool) error { killed = append(killed, pid); return nil })
	swapSetup(t, &statusPidExists, func(int) bool { return false })
	swapSetup(t, &lifecycleWritePlannedStop, func(context.Context, int) bool { return true })
	swapSetup(t, &stopSleep, func(time.Duration) {})
	reaped, err := reapUnsupervisedGatewayOrphans(context.Background(), nil, 0)
	if err != nil || !reaped || !slices.Contains(killed, orphan) || slices.Contains(killed, launchd) {
		t.Fatalf("reaped %v %v killed %v", reaped, err, killed)
	}
}

func TestReapUnsupervisedGatewayOrphansWindowsWindowsExcludesRecordedPidAndBootstrapFromKill(t *testing.T) {
	ownedStopHome(t)
	unsupervisedHost(t, "windows")
	recorded, bootstrap, orphan := 52615, 52616, 99998
	fakeParents(t, map[int]int{recorded: bootstrap, bootstrap: 0})
	swapSetup(t, &statusGetRunningPid, func(string, bool) (int, bool, error) { return recorded, true, nil })
	scanReturns(t, recorded, bootstrap, orphan)
	var killed, marked []int
	swapSetup(t, &stopKill, func(pid int, _ bool) error { killed = append(killed, pid); return nil })
	swapSetup(t, &statusPidExists, func(int) bool { return false })
	swapSetup(t, &lifecycleWritePlannedStop, func(_ context.Context, pid int) bool { marked = append(marked, pid); return true })
	swapSetup(t, &stopSleep, func(time.Duration) {})
	reaped, err := reapUnsupervisedGatewayOrphans(context.Background(), nil, 0)
	if err != nil || !reaped || !reflect.DeepEqual(marked, []int{orphan}) || len(killed) != 0 {
		t.Fatalf("reaped %v %v marked %v killed %v", reaped, err, marked, killed)
	}
}

func TestReapUnsupervisedGatewayOrphansWindowsWindowsRawRecordSuppliesExclusionWhenValidationFails(t *testing.T) {
	h := ownedStopHome(t)
	unsupervisedHost(t, "windows")
	recorded := 52615
	fakeParents(t, map[int]int{recorded: 0})
	if err := os.WriteFile(filepath.Join(h, "gateway.pid"), []byte(fmt.Sprintf(`{"pid": %d}`, recorded)), 0o600); err != nil {
		t.Fatal(err)
	}
	var probes []bool
	swapSetup(t, &statusGetRunningPid, func(_ string, cleanup bool) (int, bool, error) {
		probes = append(probes, cleanup)
		return 0, false, nil
	})
	scanReturns(t, recorded)
	var killed []int
	swapSetup(t, &stopKill, func(pid int, _ bool) error { killed = append(killed, pid); return nil })
	reaped, err := reapUnsupervisedGatewayOrphans(context.Background(), nil, 0)
	if !reflect.DeepEqual(probes, []bool{false}) {
		t.Fatalf("the fallback probe must still be non-destructive so the sweep never unlinks the record it just read, got %v", probes)
	}
	if err != nil || reaped || len(killed) != 0 {
		t.Fatalf("reaped %v %v killed %v", reaped, err, killed)
	}
}

func TestReaperStartupGraceGraceSparesBootingAndUnknownAgeButReapsStaleOrphan(t *testing.T) {
	ownedStopHome(t)
	unsupervisedHost(t, "linux")
	booting, unknown, stale := 55501, 55502, 99998
	ages := map[int]float64{booting: 2.0, stale: 900.0}
	swapSetup(t, &reapProcessAge, func(pid int) (float64, error) {
		age, ok := ages[pid]
		if !ok {
			return 0, errors.New("probe failed")
		}
		return age, nil
	})
	swapSetup(t, &statusGetRunningPid, func(string, bool) (int, bool, error) { return 0, false, nil })
	scanReturns(t, booting, unknown, stale)
	swapSetup(t, &stopKill, func(int, bool) error { return nil })
	swapSetup(t, &statusPidExists, func(int) bool { return false })
	swapSetup(t, &stopSleep, func(time.Duration) {})
	var marked []int
	swapSetup(t, &lifecycleWritePlannedStop, func(_ context.Context, pid int) bool { marked = append(marked, pid); return true })
	reaped, err := reapUnsupervisedGatewayOrphans(context.Background(), nil, 180.0)
	if err != nil || !reaped || !reflect.DeepEqual(marked, []int{stale}) {
		t.Fatalf("reaped %v %v marked %v", reaped, err, marked)
	}
}

func TestReaperCandidateIsSupervisorOwnedBackstopIsInertOnPosix(t *testing.T) {
	swapSetup(t, &serviceGOOS, "linux")
	swapSetup(t, &reapParent, func(int) (int, bool, error) {
		t.Fatal("psutil must not be consulted on POSIX")
		return 0, false, nil
	})
	if reaperCandidateIsSupervisorOwned(12345) {
		t.Fatal("supervised on POSIX")
	}
}

func TestWindowsScheduledTaskSupervisorGuardWindowsScheduledTaskRunningReturnsFalseOffWindows(t *testing.T) {
	swapSetup(t, &serviceGOOS, "linux")
	swapSetup(t, &runHiddenCommand, func(context.Context, time.Duration, ...string) (CommandResult, error) {
		t.Fatal("subprocess must not run off Windows")
		return CommandResult{}, nil
	})
	if windowsScheduledTaskSupervises(context.Background(), "HermesGateway") {
		t.Fatal("supervised off Windows")
	}
	if state, ok := windowsScheduledTaskState(context.Background(), "HermesGateway"); ok {
		t.Fatalf("state %q", state)
	}
}

func TestWindowsScheduledTaskSupervisorGuardSupervisesReadyAndQueuedButNotDisabled(t *testing.T) {
	for state, expected := range map[string]bool{"Running": true, "Ready": true, "Queued": true, "Disabled": false, "MISSING": false} {
		swapSetup(t, &reapTaskState, func(context.Context, string) (string, bool) { return state, true })
		if got := windowsScheduledTaskSupervises(context.Background(), "Icebath_Gateway"); got != expected {
			t.Fatalf("%s %v", state, got)
		}
	}
}
