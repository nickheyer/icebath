package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const wgLateRespawnS = 33.0

// Clock stand-in whose sleep alone advances time, so the poll runs as fast as the CPU allows
type wgClock struct {
	t      float64
	sleeps int
}

func wgFakeClock(t *testing.T) *wgClock {
	t.Helper()
	clock := &wgClock{}
	savedNow, savedSleep := winMonotonic, winSleep
	t.Cleanup(func() { winMonotonic, winSleep = savedNow, savedSleep })
	winMonotonic = func() float64 { return clock.t }
	winSleep = func(seconds float64) {
		if clock.sleeps++; clock.sleeps > 10000 {
			t.Fatalf("poll still running at t=%.1fs", clock.t)
		}
		clock.t += seconds
	}
	return clock
}

// Profile target whose replacement process becomes visible at a given clock time
func wgTarget(clock *wgClock, appearsAt float64, pid int, profile string) readyTarget {
	return readyTarget{Key: "profile:" + profile, ClaimAll: true, Probe: func(func() ([]int, error)) ([]int, error) {
		if clock.t >= appearsAt {
			return []int{pid}, nil
		}
		return nil, nil
	}}
}

func TestALateReplacementIsStillVerified(t *testing.T) {
	clock := wgFakeClock(t)
	window := 30.0
	ready := pollUntilReady(context.Background(), []readyTarget{wgTarget(clock, wgLateRespawnS, 4242, "default")}, 30.0, map[int]bool{}, &window)
	if !reflect.DeepEqual(ready["profile:default"], []int{4242}) {
		t.Fatalf("ready %v", ready)
	}
	if clock.t < wgLateRespawnS+readyConfirmS || clock.t > wgLateRespawnS+30.0+readyConfirmS {
		t.Fatalf("credited at t=%v", clock.t)
	}
}

func TestSharedBudgetTimesOutWhenTheReplacementIsLate(t *testing.T) {
	clock := wgFakeClock(t)
	ready := pollUntilReady(context.Background(), []readyTarget{wgTarget(clock, wgLateRespawnS, 4242, "default")}, 30.0, map[int]bool{}, nil)
	if len(ready) != 0 || clock.t < 30.0 {
		t.Fatalf("ready %v at t=%v", ready, clock.t)
	}
}

func TestAReplacementInsideTheOriginalWindowIsUnaffected(t *testing.T) {
	clock := wgFakeClock(t)
	ready := pollUntilReady(context.Background(), []readyTarget{wgTarget(clock, 23.0, 4242, "default")}, 30.0, map[int]bool{}, nil)
	if !reflect.DeepEqual(ready["profile:default"], []int{4242}) || clock.t > 30.0 {
		t.Fatalf("ready %v at t=%v", ready, clock.t)
	}
}

func TestAReplacementThatNeverAppearsStillTimesOut(t *testing.T) {
	clock := wgFakeClock(t)
	window := 30.0
	ready := pollUntilReady(context.Background(), []readyTarget{wgTarget(clock, math.Inf(1), 4242, "default")}, 30.0, map[int]bool{}, &window)
	if len(ready) != 0 {
		t.Fatalf("ready %v", ready)
	}
	if clock.t < 2*30.0 || clock.t > 2*30.0+readyConfirmS+1.0 {
		t.Fatalf("ended at t=%v", clock.t)
	}
}

func TestOneLateTargetDoesNotCostTheEarlyOneItsPass(t *testing.T) {
	clock := wgFakeClock(t)
	window := 30.0
	ready := pollUntilReady(context.Background(), []readyTarget{wgTarget(clock, 0.0, 1111, "default"), wgTarget(clock, wgLateRespawnS, 2222, "other")},
		30.0, map[int]bool{}, &window)
	if !reflect.DeepEqual(ready["profile:default"], []int{1111}) || !reflect.DeepEqual(ready["profile:other"], []int{2222}) {
		t.Fatalf("ready %v", ready)
	}
	if clock.t > wgLateRespawnS+30.0+readyConfirmS {
		t.Fatalf("ended at t=%v", clock.t)
	}
}

func TestAFlappingReplacementCannotRenewItsBudgetForever(t *testing.T) {
	clock := wgFakeClock(t)
	window := 30.0
	flapping := readyTarget{Key: "profile:default", ClaimAll: true, Probe: func(func() ([]int, error)) ([]int, error) {
		if int(clock.t)%2 == 0 {
			return []int{4242}, nil
		}
		return nil, nil
	}}
	ready := pollUntilReady(context.Background(), []readyTarget{flapping}, 30.0, map[int]bool{}, &window)
	if len(ready) != 0 {
		t.Fatalf("ready %v", ready)
	}
	if clock.t > 30.0+2*30.0+readyConfirmS+readyIntervalS+1e-6 {
		t.Fatalf("ended at t=%v", clock.t)
	}
}

func TestPendingRelaunchPids(t *testing.T) {
	cases := []struct {
		profiles map[string]any
		unmapped []any
		alive    map[int]bool
		want     []int
	}{
		{map[string]any{}, nil, map[int]bool{}, []int{}},
		{map[string]any{"default": 14980}, nil, map[int]bool{}, []int{}},
		{map[string]any{"default": 14980}, nil, map[int]bool{14980: true}, []int{14980}},
		{map[string]any{}, []any{map[string]any{"pid": 4242, "argv": nil}}, map[int]bool{4242: true}, []int{}},
		{map[string]any{"default": 14980}, []any{map[string]any{"pid": 4242, "argv": []any{"python"}}}, map[int]bool{14980: true, 4242: true}, []int{4242, 14980}},
	}
	for _, c := range cases {
		got := pendingRelaunchPids(c.profiles, c.unmapped, func(pid int) bool { return c.alive[pid] })
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("profiles %v unmapped %v: got %v want %v", c.profiles, c.unmapped, got, c.want)
		}
	}
}

func TestVerifyBudgetCoversTheWatchersOwnWaitWhenTheOldPidIsStillAlive(t *testing.T) {
	paused := map[string]any{"default": 14980}
	settled := relaunchVerifyTimeout(paused, nil, func(int) bool { return false })
	pending := relaunchVerifyTimeout(paused, nil, func(int) bool { return true })
	if pending <= gatewayRestartWatcherTimeoutS || settled >= pending {
		t.Fatalf("settled %v pending %v", settled, pending)
	}
}

// Stand-ins of the liveness poll for the bookkeeping cases, the gateway always up
func wgGatewayAlwaysUp(t *testing.T) {
	t.Helper()
	savedWait, savedLive, savedConfirm, savedAttest := winWaitForGatewayReady, winLiveGatewayPids, readyConfirmS, winWriteAttestation
	t.Cleanup(func() {
		winWaitForGatewayReady, winLiveGatewayPids, readyConfirmS, winWriteAttestation = savedWait, savedLive, savedConfirm, savedAttest
	})
	winWaitForGatewayReady = func(context.Context, *float64, string, bool, func([]int) ([]int, error)) ([]int, error) {
		return []int{4242}, nil
	}
	winLiveGatewayPids = func(context.Context, string, func([]int) ([]int, error)) ([]int, error) { return []int{4242}, nil }
	readyConfirmS = 0.0
	winWriteAttestation = func(context.Context, []int, string) {}
}

func TestMergeHelperReadsTokenKeysIntoRestartOutcome(t *testing.T) {
	wgGatewayAlwaysUp(t)
	savedResume, savedRecord := winResumeAfterUpdate, winRecordRestart
	t.Cleanup(func() { winResumeAfterUpdate, winRecordRestart = savedResume, savedRecord })
	winResumeAfterUpdate = func(context.Context, io.Writer, map[string]any) error { return nil }
	winRecordRestart = func(context.Context, updaterecord.GatewayRestart) {}
	outcome := newGatewayRestartOutcome()
	outcome.RestartedServices = []string{"icebath-gateway"}
	token := map[string]any{
		"resume_needed":       false,
		"relaunched_profiles": []any{"p1"},
		"restarted_services":  []any{"svc"},
		"service_profiles":    map[string]any{"svc": "p2", "pending": "p3"},
		"services":            []any{"pending"},
	}
	var out bytes.Buffer
	resumeWindowsGatewaysAndMergeOutcome(context.Background(), &out, outcome, token, false)
	if !reflect.DeepEqual(outcome.RelaunchedProfiles, []string{"p1", "p2"}) {
		t.Fatalf("relaunched %v", outcome.RelaunchedProfiles)
	}
	if !reflect.DeepEqual(outcome.RestartedServices, []string{"icebath-gateway", "svc"}) {
		t.Fatalf("restarted %v", outcome.RestartedServices)
	}
	if !reflect.DeepEqual(outcome.FailedOrStaleUnits, []string{"p3"}) || outcome.Incomplete {
		t.Fatalf("failed %v incomplete %v", outcome.FailedOrStaleUnits, outcome.Incomplete)
	}
}

func TestResumeUnregistersItsOwnAtexitFallbackBeforeRunning(t *testing.T) {
	wgGatewayAlwaysUp(t)
	savedUnregister, savedWindows := winUnregisterExitHook, winIsWindows
	t.Cleanup(func() { winUnregisterExitHook, winIsWindows = savedUnregister, savedWindows })
	var calls []string
	winUnregisterExitHook = func(name string) { calls = append(calls, name) }
	winIsWindows = func() bool { return false }
	token := map[string]any{"resume_needed": true}
	var out bytes.Buffer
	if err := resumeWindowsGatewaysAfterUpdate(context.Background(), &out, token); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{resumeWindowsGatewaysHook}) || token["resume_needed"] != false {
		t.Fatalf("calls %v token %v", calls, token)
	}
}

// SCM service stand-in whose process is this test process
type wgService struct{ pid int }

func (s wgService) Status() (string, error) { return "running", nil }

func (s wgService) PID() (int, error) { return s.pid, nil }

func TestServiceReadinessFilterSkipsVanishedGatewaysButNeverSwallowsBugs(t *testing.T) {
	wgGatewayAlwaysUp(t)
	child := exec.Command(os.Args[0], scChildEntry, "sleep")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	gone := exec.Command(os.Args[0], "-test.run=^$")
	_ = gone.Run()
	savedGet, savedParents := winServiceGet, winParents
	t.Cleanup(func() { winServiceGet, winParents = savedGet, savedParents })
	winServiceGet = func(string) (gatewaycli.WinService, error) { return wgService{os.Getpid()}, nil }
	winWaitForGatewayReady = func(_ context.Context, _ *float64, _ string, _ bool, filter func([]int) ([]int, error)) ([]int, error) {
		return filter([]int{child.Process.Pid, gone.Process.Pid})
	}
	zero := 0.0
	got, err := serviceGatewayReady(context.Background(), "svc", "", &zero)
	if err != nil || !reflect.DeepEqual(got, []int{child.Process.Pid}) {
		t.Fatalf("got %v err %v", got, err)
	}
	winParents = func(int) ([]int, error) {
		return nil, &pm.PyError{Class: "RuntimeError", Message: "bug in the parent walk"}
	}
	winWaitForGatewayReady = func(_ context.Context, _ *float64, _ string, _ bool, filter func([]int) ([]int, error)) ([]int, error) {
		return filter([]int{os.Getpid()})
	}
	if _, err := serviceGatewayReady(context.Background(), "svc", "", &zero); err == nil || !strings.Contains(err.Error(), "parent walk") {
		t.Fatalf("err %v", err)
	}
}

func TestThePauseRecordSurvivesExactlyWhenTheServiceRollbackFailed(t *testing.T) {
	for _, restoreFails := range []bool{false, true} {
		id := "False"
		if restoreFails {
			id = "True"
		}
		t.Run(id, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			pid := os.Getpid()
			service := gatewaycli.WindowsGatewayService{Name: "icebath-gw", Profile: "default", GatewayPID: pid, GatewayCreateTime: 1.0,
				ServicePID: pid, ServiceCreateTime: 1.0}
			savedWindows, savedPids, savedProcesses, savedServices := winIsWindows, winFindGatewayPids, winFindProfileProcesses, winFindServices
			savedStopAll, savedAttested, savedStop, savedRestore := winStopGateways, winRecordAttested, winStopService, winRestoreService
			t.Cleanup(func() {
				winIsWindows, winFindGatewayPids, winFindProfileProcesses, winFindServices = savedWindows, savedPids, savedProcesses, savedServices
				winStopGateways, winRecordAttested, winStopService, winRestoreService = savedStopAll, savedAttested, savedStop, savedRestore
			})
			winIsWindows = func() bool { return true }
			winFindGatewayPids = func(context.Context) ([]int, error) { return nil, nil }
			winFindProfileProcesses = func(context.Context) ([]gatewaycli.ProfileGatewayProcess, error) { return nil, nil }
			winFindServices = func(context.Context, []gatewaycli.ProfileGatewayProcess) ([]gatewaycli.WindowsGatewayService, error) {
				return []gatewaycli.WindowsGatewayService{service}, nil
			}
			winStopGateways = func(context.Context, io.Writer, []int, map[int]gatewaycli.ProfileGatewayProcess, map[int]bool, []int, []any,
				func(int) error, map[int]any) (map[string]any, error) {
				return map[string]any{}, nil
			}
			winRecordAttested = func(context.Context, map[string]any, map[string]bool) {}
			winStopService = func(context.Context, string, []gatewaycli.ProcessIdentity, *gatewaycli.ProcessIdentity, *gatewaycli.ProcessIdentity, float64) error {
				return &os.SyscallError{Syscall: "access denied", Err: os.ErrPermission}
			}
			winRestoreService = func(context.Context, string, float64) error {
				if restoreFails {
					return &os.SyscallError{Syscall: "service will not start", Err: os.ErrPermission}
				}
				return nil
			}
			var out bytes.Buffer
			_, err := pauseWindowsGatewaysForUpdate(context.Background(), &out)
			var failed *ServicePauseFailed
			if !errors.As(err, &failed) {
				t.Fatalf("err %v", err)
			}
			if (len(failed.RollbackFailures) > 0) != restoreFails {
				t.Fatalf("rollback failures %v", failed.RollbackFailures)
			}
			saved, err := updaterecord.ReadPauseRecord("")
			if err != nil {
				t.Fatal(err)
			}
			if (saved != nil) != restoreFails {
				t.Fatalf("record %v with restore failing %v", saved, restoreFails)
			}
		})
	}
}
