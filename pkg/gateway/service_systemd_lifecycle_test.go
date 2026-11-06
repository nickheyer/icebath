package gateway

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Swaps every lifecycle seam for the duration of the test and captures printed output
func lifecycleHarness(t *testing.T) *bytes.Buffer {
	t.Helper()
	out := &bytes.Buffer{}
	saved := []func(){}
	swap := func(restore func()) { saved = append(saved, restore) }
	o1, o2, o3, o4, o5, o6, o7, o8 := lifecycleOut, lifecycleSelectScope, lifecycleRequireInstalled, lifecyclePreflightUser, lifecycleRefreshSystemdUnit, lifecycleRunningPid, lifecycleMainPID, lifecycleRecoverPending
	swap(func() {
		lifecycleOut, lifecycleSelectScope, lifecycleRequireInstalled, lifecyclePreflightUser, lifecycleRefreshSystemdUnit, lifecycleRunningPid, lifecycleMainPID, lifecycleRecoverPending = o1, o2, o3, o4, o5, o6, o7, o8
	})
	p1, p2, p3, p4, p5, p6, p7, p8 := lifecycleStartLimited, lifecycleRunSystemctl, lifecycleServiceName, lifecycleRefuseForeign, lifecycleProbeLiveness, lifecycleRestartExitWaitBudget, lifecycleGracefulRestart, lifecycleWaitRestart
	swap(func() {
		lifecycleStartLimited, lifecycleRunSystemctl, lifecycleServiceName, lifecycleRefuseForeign, lifecycleProbeLiveness, lifecycleRestartExitWaitBudget, lifecycleGracefulRestart, lifecycleWaitRestart = p1, p2, p3, p4, p5, p6, p7, p8
	})
	q1, q2, q3, q4, q5, q6, q7, q8 := lifecycleReadProps, lifecycleSleep, lifecycleRuntimeStatus, DrainProgressReporterHook, lifecycleGeteuid, lifecycleSyncHome, lifecycleUserSystemdSocketUp, lifecycleLingerStatus
	swap(func() {
		lifecycleReadProps, lifecycleSleep, lifecycleRuntimeStatus, DrainProgressReporterHook, lifecycleGeteuid, lifecycleSyncHome, lifecycleUserSystemdSocketUp, lifecycleLingerStatus = q1, q2, q3, q4, q5, q6, q7, q8
	})
	r1, r2, r3, r4, r5, r6 := lifecycleWaitUserBus, serviceLookPath, RunServiceCommand, lifecycleGetuser, lifecycleTerminatePid, lifecycleWaitForPIDExit
	swap(func() {
		lifecycleWaitUserBus, serviceLookPath, RunServiceCommand, lifecycleGetuser, lifecycleTerminatePid, lifecycleWaitForPIDExit = r1, r2, r3, r4, r5, r6
	})
	t.Cleanup(func() {
		for _, f := range saved {
			f()
		}
	})
	lifecycleOut = out
	lifecycleSelectScope = func(context.Context, bool) (bool, error) { return false, nil }
	lifecycleRequireInstalled = func(context.Context, string, bool) error { return nil }
	lifecyclePreflightUser = func(context.Context, bool) error { return nil }
	lifecycleRefreshSystemdUnit = func(context.Context, bool) (bool, error) { return false, nil }
	lifecycleRefuseForeign = func(context.Context, string, bool) error { return nil }
	lifecycleServiceName = func(context.Context) (string, error) { return "icebath-gateway", nil }
	lifecycleRunningPid = func() (int, bool) { return 0, false }
	lifecycleMainPID = func(context.Context, bool) (int, bool, error) { return 0, false, nil }
	lifecycleProbeLiveness = func(context.Context, int, LoopLivenessOptions) string { return GatewayLoopAlive }
	lifecycleRestartExitWaitBudget = func(context.Context) (float64, error) { return 27.0, nil }
	lifecycleSleep = func(time.Duration) {}
	DrainProgressReporterHook = func(context.Context, string, float64) func() { return func() {} }
	lifecycleRunSystemctl = func(_ context.Context, args []string, _ bool, _ time.Duration) (CommandResult, error) {
		t.Fatalf("unexpected systemctl call %v", args)
		return CommandResult{}, nil
	}
	return out
}

func TestParseSystemdDurationToUS(t *testing.T) {
	cases := map[string]int64{"90s": 90_000_000, "3min": 180_000_000, "1min 30s": 90_000_000, "5": 5_000_000, "1.5s": 1_500_000, "2h": 7_200_000_000, "5 parsecs": 5_000_000}
	for raw, want := range cases {
		if got := ParseSystemdDurationToUS(raw); got == nil || got.Cmp(big.NewInt(want)) != 0 {
			t.Errorf("ParseSystemdDurationToUS(%q) = %v want %d", raw, got, want)
		}
	}
	for _, raw := range []string{"", "infinity", "0", "5parsecs", "1.2.3s", "²s"} {
		if got := ParseSystemdDurationToUS(raw); got != nil {
			t.Errorf("ParseSystemdDurationToUS(%q) = %v want nil", raw, got)
		}
	}
}

func TestSystemdRestartWaitTimeoutIncludesSupervisorBudgets(t *testing.T) {
	lifecycleHarness(t)
	lifecycleReadProps = func(_ context.Context, _ bool, props ...string) (map[string]string, error) {
		return map[string]string{"RestartUSec": "5s", "TimeoutStartUSec": "1min 30s"}, nil
	}
	if got, err := systemdRestartWaitTimeout(context.Background(), false); err != nil || got != 155.0 {
		t.Fatalf("wait timeout = %v %v want 155", got, err)
	}
}

func TestSystemdRestartTimeoutPrintsStatusGuidance(t *testing.T) {
	out := lifecycleHarness(t)
	lifecycleRecoverPending = func(context.Context, bool, int, bool) (bool, error) { return false, nil }
	lifecycleStartLimited = func(context.Context, bool) (bool, error) { return false, nil }
	lifecycleRunSystemctl = func(_ context.Context, args []string, _ bool, timeout time.Duration) (CommandResult, error) {
		if args[0] == "reset-failed" {
			return CommandResult{}, nil
		}
		return CommandResult{}, ErrCommandTimeout
	}
	if err := SystemdRestart(context.Background(), false); err != nil {
		t.Fatalf("restart surfaced %v", err)
	}
	if !strings.Contains(out.String(), "still restarting after 90s") {
		t.Fatalf("missing guidance: %q", out.String())
	}
}

func TestSystemdRestartGracefullyRestartsRunningServiceAndWaits(t *testing.T) {
	out := lifecycleHarness(t)
	var calls []string
	lifecycleRefreshSystemdUnit = func(_ context.Context, system bool) (bool, error) {
		calls = append(calls, "refresh")
		return false, nil
	}
	lifecycleRunningPid = func() (int, bool) { return 654, true }
	lifecycleGracefulRestart = func(pid int, budget float64, _ func()) bool {
		if pid != 654 || budget != 27.0 {
			t.Fatalf("graceful got pid %d budget %v", pid, budget)
		}
		calls = append(calls, "graceful")
		return true
	}
	lifecycleWaitRestart = func(_ context.Context, system bool, prev int, have bool, _ float64, _ *bool) (bool, error) {
		if system || prev != 654 || !have {
			t.Fatalf("wait got system %v prev %d", system, prev)
		}
		calls = append(calls, "wait")
		return true, nil
	}
	if err := SystemdRestart(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "refresh,graceful,wait" {
		t.Fatalf("calls %v", calls)
	}
	text := strings.ToLower(out.String())
	if !strings.Contains(text, "restarting gracefully") || !strings.Contains(text, "27") || strings.Contains(text, "21627") {
		t.Fatalf("output %q", text)
	}
}

// Wires a graceful restart whose replacement wait fails with the given properties
func gracefulNoReplacement(t *testing.T, props map[string]string, waits ...bool) *[]string {
	var calls []string
	lifecycleRunningPid = func() (int, bool) { return 654, true }
	lifecycleGracefulRestart = func(int, float64, func()) bool { return true }
	i := 0
	lifecycleWaitRestart = func(context.Context, bool, int, bool, float64, *bool) (bool, error) {
		w := waits[i]
		i++
		return w, nil
	}
	lifecycleStartLimited = func(context.Context, bool) (bool, error) { return false, nil }
	lifecycleReadProps = func(context.Context, bool, ...string) (map[string]string, error) { return props, nil }
	lifecycleRunSystemctl = func(_ context.Context, args []string, _ bool, _ time.Duration) (CommandResult, error) {
		calls = append(calls, args[0])
		return CommandResult{}, nil
	}
	return &calls
}

func TestSystemdRestartForcesRecoveryOnlyWhenHandoffHasNoReplacement(t *testing.T) {
	out := lifecycleHarness(t)
	calls := gracefulNoReplacement(t, map[string]string{"ActiveState": "inactive", "MainPID": "0"}, false, true)
	if err := SystemdRestart(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*calls, ",") != "reset-failed,start" || !strings.Contains(out.String(), "did not relaunch") {
		t.Fatalf("calls %v out %q", *calls, out.String())
	}
}

func TestSystemdRestartDoesNotForceAnUnreadyReplacement(t *testing.T) {
	lifecycleHarness(t)
	calls := gracefulNoReplacement(t, map[string]string{"ActiveState": "active", "MainPID": "777"}, false)
	if err := SystemdRestart(context.Background(), false); err != nil || len(*calls) != 0 {
		t.Fatalf("calls %v err %v", *calls, err)
	}
}

func TestSystemdRestartDoesNotRecoverAFailedReplacement(t *testing.T) {
	lifecycleHarness(t)
	calls := gracefulNoReplacement(t, map[string]string{"ActiveState": "inactive", "MainPID": "0"}, false)
	lifecycleWaitRestart = func(_ context.Context, _ bool, _ int, _ bool, _ float64, observed *bool) (bool, error) {
		*observed = true
		return false, nil
	}
	if err := SystemdRestart(context.Background(), false); err != nil || len(*calls) != 0 {
		t.Fatalf("calls %v err %v", *calls, err)
	}
}

func TestSystemdRestartDoesNotRecoverWhenHandoffStateIsUnknown(t *testing.T) {
	lifecycleHarness(t)
	calls := gracefulNoReplacement(t, map[string]string{}, false)
	if err := SystemdRestart(context.Background(), false); err != nil || len(*calls) != 0 {
		t.Fatalf("calls %v err %v", *calls, err)
	}
}

func TestWaitAcceptsADegradedReplacementAsRestarted(t *testing.T) {
	out := lifecycleHarness(t)
	lifecycleReadProps = func(context.Context, bool, ...string) (map[string]string, error) {
		return map[string]string{"ActiveState": "active", "MainPID": "777"}, nil
	}
	lifecycleRunningPid = func() (int, bool) { return 777, true }
	lifecycleRuntimeStatus = func() *pyjson.Object {
		o := pyjson.NewObject()
		o.Set("pid", int64(777))
		o.Set("gateway_state", "degraded")
		return o
	}
	ok, err := waitForSystemdServiceRestart(context.Background(), false, 654, true, 1.0, nil)
	if err != nil || !ok || !strings.Contains(out.String(), "DEGRADED") {
		t.Fatalf("ok %v err %v out %q", ok, err, out.String())
	}
}

func TestWedgedGatewayRestartEscalatesInsteadOfDraining(t *testing.T) {
	lifecycleHarness(t)
	var calls []string
	lifecycleRunningPid = func() (int, bool) { return 4242, true }
	lifecycleProbeLiveness = func(context.Context, int, LoopLivenessOptions) string { return GatewayLoopWedged }
	old := lifecycleEscalate
	t.Cleanup(func() { lifecycleEscalate = old })
	lifecycleEscalate = func(pid int, term, kill float64) bool {
		calls = append(calls, "escalate")
		return true
	}
	lifecycleGracefulRestart = func(int, float64, func()) bool { t.Fatal("drained a wedged gateway"); return false }
	lifecycleRunSystemctl = func(_ context.Context, args []string, _ bool, _ time.Duration) (CommandResult, error) {
		calls = append(calls, args[0])
		return CommandResult{}, nil
	}
	lifecycleWaitRestart = func(_ context.Context, _ bool, prev int, _ bool, _ float64, _ *bool) (bool, error) {
		calls = append(calls, "wait")
		return true, nil
	}
	if err := SystemdRestart(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "escalate,reset-failed,restart,wait" {
		t.Fatalf("calls %v", calls)
	}
}

func TestPreflightAcceptsPrivateSocketWithoutDbusBus(t *testing.T) {
	lifecycleHarness(t)
	lifecycleUserSystemdSocketUp = userSystemdSocketReady
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "systemd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "systemd", "private"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	if !userSystemdSocketReady() {
		t.Skip("runtime dir override is not honoured on this host")
	}
	if err := preflightUserSystemd(context.Background(), false); err != nil {
		t.Fatalf("preflight refused the private socket: %v", err)
	}
	if !waitForUserDbusSocket(100 * time.Millisecond) {
		t.Fatal("wait refused the private socket")
	}
}

// Prepares a user scope with no bus, linger off and loginctl answering with res
func noBusLingerOff(t *testing.T, res CommandResult) {
	lifecycleUserSystemdSocketUp = func() bool { return false }
	off := false
	lifecycleLingerStatus = func(context.Context, string) (*bool, string) { return &off, "" }
	serviceLookPath = func(string) (string, error) { return "/usr/bin/loginctl", nil }
	lifecycleGetuser = func() (string, error) { return "rick", nil }
	RunServiceCommand = func(_ context.Context, _ time.Duration, argv ...string) (CommandResult, error) { return res, nil }
}

func TestPreflightRaisesWhenLingerDisabledAndLoginctlDenied(t *testing.T) {
	lifecycleHarness(t)
	noBusLingerOff(t, CommandResult{ReturnCode: 1, Stderr: "Interactive authentication required."})
	err := preflightUserSystemd(context.Background(), true)
	var une *UserSystemdUnavailableError
	if !errors.As(err, &une) {
		t.Fatalf("want UserSystemdUnavailableError, got %v", err)
	}
	for _, want := range []string{"sudo loginctl enable-linger", "icebath gateway run", "Interactive authentication required"} {
		if !strings.Contains(une.Msg, want) {
			t.Errorf("message lacks %q: %s", want, une.Msg)
		}
	}
}

func TestPreflightEnablesLingerAndSocketAppears(t *testing.T) {
	out := lifecycleHarness(t)
	noBusLingerOff(t, CommandResult{})
	lifecycleWaitUserBus = func(time.Duration) bool { return true }
	if err := preflightUserSystemd(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Enabled linger") {
		t.Fatalf("output %q", out.String())
	}
}

func TestNonRootSystemStartExitsWithCleanMessage(t *testing.T) {
	lifecycleHarness(t)
	lifecycleSelectScope = func(context.Context, bool) (bool, error) { return true, nil }
	lifecycleGeteuid = func() int { return 1000 }
	err := SystemdStart(context.Background(), true)
	var root *SystemScopeRequiresRootError
	if !errors.As(err, &root) || !strings.Contains(root.Error(), "requires root") || strings.Contains(root.Error(), "('") || root.Action != "start" {
		t.Fatalf("got %v", err)
	}
}

func TestSystemdStopTimeoutPrintsGuidanceAndMarksPlannedStop(t *testing.T) {
	out := lifecycleHarness(t)
	lifecycleSyncHome = func(context.Context, bool) error { return nil }
	marked := 0
	oldW, oldR := lifecycleWritePlannedStop, lifecycleRunningPidNoCleanup
	t.Cleanup(func() { lifecycleWritePlannedStop, lifecycleRunningPidNoCleanup = oldW, oldR })
	lifecycleRunningPidNoCleanup = func() (int, bool) { return 99, true }
	lifecycleWritePlannedStop = func(pid int) bool { marked = pid; return true }
	lifecycleRunSystemctl = func(context.Context, []string, bool, time.Duration) (CommandResult, error) {
		return CommandResult{}, ErrCommandTimeout
	}
	if err := SystemdStop(context.Background(), false); err != nil || marked != 99 || !strings.Contains(out.String(), "still stopping after 90s") {
		t.Fatalf("err %v marked %d out %q", err, marked, out.String())
	}
}

func TestStartLimitRejectionPrintsWaitHint(t *testing.T) {
	out := lifecycleHarness(t)
	lifecycleRunSystemctl = func(_ context.Context, args []string, _ bool, _ time.Duration) (CommandResult, error) {
		if args[0] == "reset-failed" {
			return CommandResult{}, nil
		}
		return CommandResult{ReturnCode: 1, Stderr: "Job failed: start request repeated too quickly"}, nil
	}
	if err := systemdResetAndRun(context.Background(), "restart", false, 0, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rate-limited by systemd") || !strings.Contains(out.String(), "systemctl --user reset-failed icebath-gateway") {
		t.Fatalf("output %q", out.String())
	}
	if !systemdUnitIsStartLimited(map[string]string{"Result": "Start-Limit-Hit"}) || systemdUnitIsStartLimited(map[string]string{"Result": "success"}) {
		t.Fatal("start limit detection")
	}
}

func TestRecoverPendingRestartClearsFailedPlannedExit(t *testing.T) {
	out := lifecycleHarness(t)
	lifecycleReadProps = func(context.Context, bool, ...string) (map[string]string, error) {
		return map[string]string{"ActiveState": "failed", "ExecMainStatus": "75"}, nil
	}
	lifecycleRuntimeStatus = func() *pyjson.Object {
		o := pyjson.NewObject()
		o.Set("restart_requested", true)
		return o
	}
	var calls []string
	lifecycleRunSystemctl = func(_ context.Context, args []string, _ bool, _ time.Duration) (CommandResult, error) {
		calls = append(calls, args[0])
		return CommandResult{}, nil
	}
	lifecycleWaitRestart = func(context.Context, bool, int, bool, float64, *bool) (bool, error) { return true, nil }
	ok, err := recoverPendingSystemdRestart(context.Background(), false, 0, false)
	if err != nil || !ok || strings.Join(calls, ",") != "reset-failed,start" || !strings.Contains(out.String(), "Clearing failed state") {
		t.Fatalf("ok %v err %v calls %v", ok, err, calls)
	}
	lifecycleRuntimeStatus = func() *pyjson.Object { return pyjson.NewObject() }
	if ok, _ := recoverPendingSystemdRestart(context.Background(), false, 0, false); ok {
		t.Fatal("recovered without a requested restart")
	}
}
