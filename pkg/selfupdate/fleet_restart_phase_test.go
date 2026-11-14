package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

func TestSurvivingGatewayProbeUndeterminableWhenGatewayModuleIsBroken(t *testing.T) {
	vsSwap(t, &fleetFindGatewayPIDs, func(context.Context, map[int]bool, bool) ([]int, error) {
		return nil, errors.New("cannot import name 'is_trivial_prompt'")
	})
	if pids, known := survivingGatewayPidsAfterFailedRestart(context.Background()); known || pids != nil {
		t.Fatalf("pids=%v known=%v", pids, known)
	}
}

func TestRestartPhaseFailureIsIncompleteStaleWhenAGatewayStillSurvives(t *testing.T) {
	if !restartPhaseFailureIsIncomplete([]int{4321}, true, []int{4321}) {
		t.Fatal("a surviving gateway read as safe")
	}
}

func TestRestartPhaseFailureIsIncompleteStaleWhenSurvivorProbeIsUndeterminable(t *testing.T) {
	if !restartPhaseFailureIsIncomplete(nil, false, []int{}) {
		t.Fatal("an undeterminable probe read as safe")
	}
}

func TestRestartPhaseFailureIsIncompleteStaleWhenPreexistingGatewayStoppedWithoutReplacement(t *testing.T) {
	if !restartPhaseFailureIsIncomplete([]int{}, true, []int{4321}) {
		t.Fatal("a stopped gateway without a replacement read as safe")
	}
}

func TestRestartPhaseFailureIsIncompleteStaleWhenPreRestartStateCouldNotBeRead(t *testing.T) {
	if !restartPhaseFailureIsIncomplete([]int{}, true, nil) {
		t.Fatal("an unreadable pre restart state read as safe")
	}
}

func TestRestartPhaseFailureIsIncompleteCleanOnlyWhenNothingRanBeforeAndNoneSurvive(t *testing.T) {
	if restartPhaseFailureIsIncomplete([]int{}, true, []int{}) {
		t.Fatal("an idle host read as stale")
	}
}

func TestAbortedRestartWarningWarnsWithRecoveryCommandAndCause(t *testing.T) {
	var out bytes.Buffer
	warnGatewayRestartPhaseAborted(&out, errors.New("cannot import name 'is_trivial_prompt'"), []int{4321})
	for _, want := range []string{"Update incomplete", "is_trivial_prompt", "4321", "icebath gateway restart"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("%q missing from %q", want, out.String())
		}
	}
}

func TestAbortedRestartWarningWarnsEvenWhenSurvivingPidsAreUnknown(t *testing.T) {
	var out bytes.Buffer
	warnGatewayRestartPhaseAborted(&out, errors.New("systemctl exploded"), nil)
	for _, want := range []string{"Update incomplete", "systemctl exploded", "icebath gateway restart"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("%q missing from %q", want, out.String())
		}
	}
}

func planOfKinds(kinds ...string) *UpdatePlan {
	plan := NewUpdatePlan()
	for _, kind := range kinds {
		plan.Runtimes = append(plan.Runtimes, &RuntimeRecord{Kind: kind, Profile: "default", Supervisor: "manual"})
	}
	return plan
}

func TestResumeTokenIsNotARuntimeTokenWithEmptyPidSnapshotIsStillNotExpected(t *testing.T) {
	token := map[string]any{"resume_needed": true, "profiles": map[string]any{"work": 777}, "unmapped": []any{}}
	if fleetProbeExpectedRuntimes(planOfKinds(), []int{}, token, []string{}, map[int]bool{}) {
		t.Fatal("a resume token alone predicted rows")
	}
}

func TestRowCapableSignalsStillCountPreRestartPidsStillExpectRowsAlongsideToken(t *testing.T) {
	token := map[string]any{"resume_needed": false, "profiles": map[string]any{"default": 4321}}
	if !fleetProbeExpectedRuntimes(nil, []int{4321}, token, []string{}, map[int]bool{}) {
		t.Fatal("a pre restart pid predicted no rows")
	}
}

func TestRowCapableSignalsStillCountPlanInventoryStillExpectsRowsAlongsideToken(t *testing.T) {
	token := map[string]any{"resume_needed": false, "unmapped": []any{map[string]any{"pid": 99, "argv": []any{"x"}}}}
	if !fleetProbeExpectedRuntimes(planOfKinds("gateway"), []int{}, token, []string{}, map[int]bool{}) {
		t.Fatal("a planned gateway predicted no rows")
	}
}

func TestExpectedRows(t *testing.T) {
	none := []string(nil)
	cases := []struct {
		plan     *UpdatePlan
		pids     []int
		token    any
		services []string
		killed   map[int]bool
		expected bool
	}{
		{nil, []int{}, map[string]any{"profiles": map[string]any{"default": 4321}}, none, nil, false},
		{nil, []int{}, map[string]any{"unmapped": []any{map[string]any{"pid": 99}}}, none, nil, false},
		{planOfKinds(), []int{}, map[string]any{"profiles": map[string]any{"work": 777}}, none, nil, false},
		{nil, []int{}, map[string]any{"services": []any{"HermesGateway"}}, none, nil, false},
		{nil, []int{}, map[string]any{}, none, nil, false},
		{nil, []int{}, nil, none, nil, false},
		{planOfKinds(), []int{}, nil, none, nil, false},
		{nil, nil, nil, none, nil, true},
		{nil, []int{4321}, map[string]any{"profiles": map[string]any{"default": 4321}}, none, nil, true},
		{planOfKinds("gateway"), []int{}, map[string]any{"unmapped": []any{map[string]any{"pid": 99}}}, none, nil, true},
		{planOfKinds("serve", "dashboard"), []int{}, nil, none, nil, false},
		{planOfKinds("serve", "gateway"), []int{}, nil, none, nil, true},
		{nil, []int{}, nil, []string{"icebath-gateway"}, nil, true},
		{nil, []int{}, nil, none, map[int]bool{4321: true}, true},
	}
	for i, tc := range cases {
		if got := fleetProbeExpectedRuntimes(tc.plan, tc.pids, tc.token, tc.services, tc.killed); got != tc.expected {
			t.Fatalf("case %d: expected rows = %v, want %v", i, got, tc.expected)
		}
	}
}

func listUnitsStdout(names ...string) string {
	lines := make([]string, len(names))
	for i, name := range names {
		lines[i] = name + ".service loaded active running"
	}
	return strings.Join(lines, "\n")
}

// Unit names a list-units output hands to the restart loop, failing on any timeout callback
func seenGatewayUnits(t *testing.T, stdout string) []string {
	t.Helper()
	seen := []string{}
	err := forEachSystemdGatewayUnit(stdout, func(name string) error { seen = append(seen, name); return nil },
		func(string, error) { t.Fatal("unexpected timeout") })
	if err != nil {
		t.Fatal(err)
	}
	return seen
}

func TestFleetRestartTimeoutIsolationTimeoutOnMiddleUnitContinuesRemainingUnits(t *testing.T) {
	units := []string{"icebath-gateway-xiaomo1", "icebath-gateway-xiaomo2", "icebath-gateway-xiaomo3", "icebath-gateway-xiaomo4",
		"icebath-gateway-xiaomo5", "icebath-gateway-xiaomo6", "icebath-gateway-xiaomo7", "icebath-gateway"}
	restarted, failed, timeoutCmds := []string{}, []string{}, [][]string{}
	err := forEachSystemdGatewayUnit(listUnitsStdout(units...), func(svcName string) error {
		if svcName == "icebath-gateway-xiaomo5" {
			return &pysubprocess.TimeoutExpired{Cmd: []string{"systemctl", "--user", "--no-ask-password", "restart", svcName}, Timeout: 15}
		}
		restarted = append(restarted, svcName)
		return nil
	}, func(svcName string, timeout error) {
		failed = append(failed, svcName)
		timeoutCmds = append(timeoutCmds, asCommandTimeout(timeout).Cmd)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantRestarted := append(append([]string{}, units[:4]...), units[5:]...)
	if !reflect.DeepEqual(failed, []string{"icebath-gateway-xiaomo5"}) || !reflect.DeepEqual(restarted, wantRestarted) {
		t.Fatalf("restarted=%v failed=%v", restarted, failed)
	}
	if want := [][]string{{"systemctl", "--user", "--no-ask-password", "restart", "icebath-gateway-xiaomo5"}}; !reflect.DeepEqual(timeoutCmds, want) {
		t.Fatalf("timeout commands = %v", timeoutCmds)
	}
}

func TestFleetRestartTimeoutIsolationNonGatewayUnitsInListOutputAreIgnored(t *testing.T) {
	seen := seenGatewayUnits(t, "ssh.service loaded active running\nicebath-gateway-coder.service loaded active running\nnot-a-service loaded active running\n")
	if !reflect.DeepEqual(seen, []string{"icebath-gateway-coder"}) {
		t.Fatalf("seen = %v", seen)
	}
}

func TestFleetRestartTimeoutIsolationHermesServeUnitsAreIncluded(t *testing.T) {
	seen := seenGatewayUnits(t, "ssh.service loaded active running\nicebath-serve.service loaded active running\n"+
		"icebath-serve-work.service loaded active running\nicebath-gateway.service loaded active running\n")
	if !reflect.DeepEqual(seen, []string{"icebath-serve", "icebath-serve-work", "icebath-gateway"}) {
		t.Fatalf("seen = %v", seen)
	}
}

func TestFleetRestartTimeoutIsolationHermesDashboardUnitsAreIncluded(t *testing.T) {
	seen := seenGatewayUnits(t, "ssh.service loaded active running\nicebath-dashboard.service loaded active running\n"+
		"icebath-dashboard-work.service loaded active running\nicebath-serve.service loaded active running\n")
	if !reflect.DeepEqual(seen, []string{"icebath-dashboard", "icebath-dashboard-work", "icebath-serve"}) {
		t.Fatalf("seen = %v", seen)
	}
}

func TestFleetRestartTimeoutIsolationHermesDashboardNearPrefixIsRejected(t *testing.T) {
	if seen := seenGatewayUnits(t, listUnitsStdout("icebath-dashboardd", "icebath-dashboard-work")); !reflect.DeepEqual(seen, []string{"icebath-dashboard-work"}) {
		t.Fatalf("seen = %v", seen)
	}
}

func TestFleetRestartTimeoutIsolationHermesServerNearPrefixIsRejected(t *testing.T) {
	if seen := seenGatewayUnits(t, listUnitsStdout("icebath-server")); len(seen) != 0 {
		t.Fatalf("seen = %v", seen)
	}
}

func TestFleetRestartTimeoutIsolationHermesGatewayNearPrefixIsRejected(t *testing.T) {
	if seen := seenGatewayUnits(t, listUnitsStdout("icebath-gatewayd", "icebath-gateway-coder")); !reflect.DeepEqual(seen, []string{"icebath-gateway-coder"}) {
		t.Fatalf("seen = %v", seen)
	}
}

func TestGracefulSigusr1EligibilityGatewayUnitsAreEligible(t *testing.T) {
	if !serviceUnitSupportsGracefulSigusr1Restart("icebath-gateway") || !serviceUnitSupportsGracefulSigusr1Restart("icebath-gateway-work") {
		t.Fatal("a gateway unit was not eligible")
	}
}

func TestGracefulSigusr1EligibilityServeUnitsAreNotEligible(t *testing.T) {
	if serviceUnitSupportsGracefulSigusr1Restart("icebath-serve") || serviceUnitSupportsGracefulSigusr1Restart("icebath-serve-work") {
		t.Fatal("a serve unit was eligible")
	}
}

func TestGracefulSigusr1EligibilityProcessErrorsOtherThanTimeoutStillPropagate(t *testing.T) {
	err := forEachSystemdGatewayUnit(listUnitsStdout("icebath-gateway"), func(string) error { return errors.New("not a timeout") },
		func(string, error) { t.Fatal("timeout handler must not run") })
	if err == nil || !strings.Contains(err.Error(), "not a timeout") {
		t.Fatalf("err = %v", err)
	}
}

func TestIncompleteFleetRestartWarningWarnsWithExactUnrestartedUnits(t *testing.T) {
	var out bytes.Buffer
	warnIncompleteGatewayFleetRestart(&out, []string{"icebath-gateway-xiaomo5", "icebath-gateway-xiaomo6", "icebath-gateway-xiaomo5"})
	text := out.String()
	if !strings.Contains(text, "Update incomplete") || strings.Count(text, "icebath-gateway-xiaomo5") != 1 ||
		!strings.Contains(text, "icebath-gateway-xiaomo6") || !strings.Contains(text, "pre-update code") {
		t.Fatalf("out = %q", text)
	}
}

// Fakes the invoking profile's launchd job, its plist present unless told otherwise
type ownLaunchdJob struct {
	restarts, verifies int
	verifiedLabel      string
	commands           [][]string
}

func ownLaunchd(t *testing.T, plistExists bool, restartErr error, supervised bool) *ownLaunchdJob {
	t.Helper()
	job := &ownLaunchdJob{}
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	plist := filepath.Join(tmp, "ai.icebath.gateway.plist")
	if plistExists {
		if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	vsSwap(t, &fleetLaunchdLabel, func(context.Context) (string, error) { return "ai.icebath.gateway", nil })
	vsSwap(t, &fleetLaunchdPlistPath, func(context.Context) (string, error) { return plist, nil })
	vsSwap(t, &fleetLaunchdRestart, func(context.Context) error {
		if restartErr != nil {
			return restartErr
		}
		job.restarts++
		return nil
	})
	vsSwap(t, &fleetRunCommand, func(_ context.Context, cmd []string, _ float64) (*pysubprocess.Completed, error) {
		job.commands = append(job.commands, cmd)
		return &pysubprocess.Completed{Args: cmd}, nil
	})
	vsSwap(t, &fleetLaunchdLabelsForInstall, func(context.Context) ([]string, error) { return []string{"ai.icebath.gateway"}, nil })
	vsSwap(t, &fleetIsPidAncestor, func(int) bool { return false })
	vsSwap(t, &fleetWaitLaunchdSupervised, func(_ context.Context, label string, _ int, _ bool) (bool, error) {
		job.verifies++
		job.verifiedLabel = label
		return supervised, nil
	})
	return job
}

func ownLaunchdRestart(t *testing.T, w io.Writer) ([]string, []string) {
	t.Helper()
	restarted, failed, err := restartLaunchdGatewayAfterUpdate(context.Background(), w, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return restarted, failed
}

func TestLaunchdRestartAfterUpdatePlistPresentAlwaysRestartsWithoutClassifying(t *testing.T) {
	job := ownLaunchd(t, true, nil, true)
	var out bytes.Buffer
	restarted, failed := ownLaunchdRestart(t, &out)
	if !reflect.DeepEqual(restarted, []string{"ai.icebath.gateway"}) || len(failed) != 0 || job.restarts != 1 || len(job.commands) != 0 ||
		strings.Contains(out.String(), "NOT running") {
		t.Fatalf("restarted=%v failed=%v job=%+v out=%q", restarted, failed, job, out.String())
	}
}

func TestLaunchdRestartAfterUpdateRestartFailureWarnsThatGatewayIsDown(t *testing.T) {
	ownLaunchd(t, true, &gateway.CalledProcessError{Cmd: []string{"launchctl", "kickstart"}, ReturnCode: 1, Stderr: "kickstart refused"}, true)
	var out bytes.Buffer
	restarted, failed := ownLaunchdRestart(t, &out)
	if len(restarted) != 0 || !reflect.DeepEqual(failed, []string{"ai.icebath.gateway"}) ||
		!strings.Contains(out.String(), "kickstart refused") || !strings.Contains(out.String(), "icebath gateway restart") {
		t.Fatalf("restarted=%v failed=%v out=%q", restarted, failed, out.String())
	}
}

func TestLaunchdRestartAfterUpdateLaunchctlUnusableIsNotSwallowed(t *testing.T) {
	failures := map[string]error{
		"missing": fmt.Errorf("%w: %q", gateway.ErrCommandNotFound, "launchctl"),
		"timeout": &pysubprocess.TimeoutExpired{Cmd: []string{"launchctl", "kickstart"}, Timeout: 90},
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			job := ownLaunchd(t, true, failure, true)
			var out bytes.Buffer
			restarted, failed := ownLaunchdRestart(t, &out)
			if len(restarted) != 0 || !reflect.DeepEqual(failed, []string{"ai.icebath.gateway"}) || job.restarts != 0 ||
				!strings.Contains(out.String(), "icebath gateway restart") {
				t.Fatalf("restarted=%v failed=%v out=%q", restarted, failed, out.String())
			}
		})
	}
}

func TestLaunchdRestartAfterUpdateNoPlistIsNotALaunchdInstall(t *testing.T) {
	job := ownLaunchd(t, false, nil, true)
	var out bytes.Buffer
	restarted, failed := ownLaunchdRestart(t, &out)
	if len(restarted) != 0 || len(failed) != 0 || job.restarts != 0 || out.Len() != 0 {
		t.Fatalf("restarted=%v failed=%v out=%q", restarted, failed, out.String())
	}
}

func runOwnFleetRestart(t *testing.T, w io.Writer) ([]string, []string) {
	t.Helper()
	restarted, failed := []string{}, []string{}
	if err := restartMacosLaunchdGateways(context.Background(), w, &restarted, &failed, 5.0, false, nil); err != nil {
		t.Fatal(err)
	}
	return restarted, failed
}

func TestInvokingProfileIsVerifiedLikeItsSiblingsReportsRestartedOnlyAfterSupervisionIsConfirmed(t *testing.T) {
	job := ownLaunchd(t, true, nil, true)
	restarted, failed := runOwnFleetRestart(t, io.Discard)
	if !reflect.DeepEqual(restarted, []string{"ai.icebath.gateway"}) || len(failed) != 0 || job.restarts != 1 || job.verifies != 1 ||
		job.verifiedLabel != "ai.icebath.gateway" {
		t.Fatalf("restarted=%v failed=%v job=%+v", restarted, failed, job)
	}
}

func TestInvokingProfileIsVerifiedLikeItsSiblingsUnverifiedRestartIsNotReportedAsRestarted(t *testing.T) {
	ownLaunchd(t, true, nil, false)
	var out bytes.Buffer
	restarted, failed := runOwnFleetRestart(t, &out)
	if len(restarted) != 0 || !reflect.DeepEqual(failed, []string{"ai.icebath.gateway"}) || !strings.Contains(out.String(), "ai.icebath.gateway") {
		t.Fatalf("restarted=%v failed=%v out=%q", restarted, failed, out.String())
	}
}

func TestInvokingProfileIsVerifiedLikeItsSiblingsRaisedRestartFailureIsNotVerifiedAndIsReported(t *testing.T) {
	job := ownLaunchd(t, true, &gateway.CalledProcessError{Cmd: []string{"launchctl"}, ReturnCode: 1, Stderr: "boom"}, true)
	var out bytes.Buffer
	restarted, failed := runOwnFleetRestart(t, &out)
	if len(restarted) != 0 || !reflect.DeepEqual(failed, []string{"ai.icebath.gateway"}) || job.verifies != 0 || !strings.Contains(out.String(), "boom") {
		t.Fatalf("restarted=%v failed=%v job=%+v out=%q", restarted, failed, job, out.String())
	}
}

func TestInvokingProfileIsVerifiedLikeItsSiblingsNoPlistMeansTheGatewayIsNotLaunchdManaged(t *testing.T) {
	job := ownLaunchd(t, false, nil, true)
	restarted, failed := runOwnFleetRestart(t, io.Discard)
	if len(restarted) != 0 || len(failed) != 0 || job.restarts != 0 || job.verifies != 0 {
		t.Fatalf("restarted=%v failed=%v job=%+v", restarted, failed, job)
	}
}

func TestInvokingProfileIsVerifiedLikeItsSiblingsUnregisteredLabelIsRestartedNotSkipped(t *testing.T) {
	job := ownLaunchd(t, true, nil, true)
	restarted, failed := runOwnFleetRestart(t, io.Discard)
	if !reflect.DeepEqual(restarted, []string{"ai.icebath.gateway"}) || len(failed) != 0 || job.restarts != 1 || job.verifies != 1 {
		t.Fatalf("restarted=%v failed=%v job=%+v", restarted, failed, job)
	}
}
