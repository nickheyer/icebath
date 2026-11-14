package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Monotonic clock a test advances only by sleeping
type fleetFakeClock struct {
	base   time.Time
	now    time.Duration
	sleeps []time.Duration
}

func installFleetClock(t *testing.T) *fleetFakeClock {
	t.Helper()
	clock := &fleetFakeClock{base: time.Unix(1_700_000_000, 0)}
	vsSwap(t, &fleetMonotonic, func() time.Time { return clock.base.Add(clock.now) })
	vsSwap(t, &fleetSleep, func(d time.Duration) {
		clock.sleeps = append(clock.sleeps, d)
		clock.now += d
	})
	return clock
}

func fleetObject(pairs ...any) *pyjson.Object { return pyjson.ObjectOf(pairs...) }

func fleetJSON(t *testing.T, value any) string {
	t.Helper()
	text, err := pyjson.Dumps(value)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func completedText(cmd []string, stdout string) *pysubprocess.Completed {
	return &pysubprocess.Completed{Args: cmd, Stdout: []byte(stdout), Stderr: []byte{}}
}

func readLatestReceipt(t *testing.T, home string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "logs", "update_receipts", "latest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func followupSteps(receipt map[string]any) []string {
	steps := []string{}
	rows, _ := receipt["followups"].([]any)
	for _, row := range rows {
		steps = append(steps, row.(map[string]any)["step"].(string))
	}
	return steps
}

func TestLaunchdLabelsGetLaunchctlHint(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd arm of the warning")
	}
	var out bytes.Buffer
	warnIncompleteGatewayFleetRestart(&out, []string{"ai.icebath.gateway-merit-ops"})
	if !strings.Contains(out.String(), "Update incomplete") || !strings.Contains(out.String(), "launchctl kickstart -k") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestSystemdUnitsKeepSystemctlHint(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd arm of the warning")
	}
	var out bytes.Buffer
	warnIncompleteGatewayFleetRestart(&out, []string{"icebath-gateway-coder"})
	if !strings.Contains(out.String(), "systemctl") || strings.Contains(out.String(), "launchctl") {
		t.Fatalf("out = %q", out.String())
	}
}

const fleetForeignHome = "/srv/other-account-home/.icebath"

func fleetOwnHome(t *testing.T) string {
	t.Helper()
	own := filepath.Join(t.TempDir(), "homeA", ".icebath")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", own)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", t.TempDir())
	return own
}

func fleetPidHomes(t *testing.T, mapping map[int]string) {
	t.Helper()
	vsSwap(t, &fleetHomeForPID, func(_ context.Context, pid int) (string, bool, error) {
		found, ok := mapping[pid]
		return found, ok && found != "", nil
	})
	vsSwap(t, &fleetPartitionPidsByScope, func(ctx context.Context, pids []int, scope map[string]bool) ([]int, []gateway.ForeignGatewayPid) {
		owned, foreign := []int{}, []gateway.ForeignGatewayPid{}
		for _, pid := range pids {
			if found := mapping[pid]; found != "" && gateway.HomeInUpdateScope(ctx, found, scope) {
				owned = append(owned, pid)
			} else {
				foreign = append(foreign, gateway.ForeignGatewayPid{PID: pid, Home: found})
			}
		}
		return owned, foreign
	})
}

func TestSystemdUnitOfAnotherHomeIsLeftAlone(t *testing.T) {
	own := fleetOwnHome(t)
	fleetPidHomes(t, map[int]string{4242: fleetForeignHome, 4343: own})
	mainPID := 4242
	writeVerbs := [][]string{}
	vsSwap(t, &fleetSystemctl, func(_ context.Context, cmd []string, _ float64) (*pysubprocess.Completed, error) {
		switch {
		case slices.Contains(cmd, "is-active"):
			return completedText(cmd, "active\n"), nil
		case slices.Contains(cmd, "show") && slices.Contains(cmd, "--property=MainPID"):
			return completedText(cmd, strconv.Itoa(mainPID)+"\n"), nil
		case slices.Contains(cmd, "show"):
			return completedText(cmd, ""), nil
		}
		writeVerbs = append(writeVerbs, cmd)
		return completedText(cmd, ""), nil
	})
	vsSwap(t, &fleetRepairUnit, func(context.Context, io.Writer, string, string) error { return nil })
	drained := []int{}
	vsSwap(t, &fleetDrainOrSignal, func(_ context.Context, _ io.Writer, pid int, _ float64, _ string, _ ...map[int]bool) (bool, error) {
		drained = append(drained, pid)
		return true, nil
	})
	vsSwap(t, &fleetWaitServiceActive, func(context.Context, []string, string, float64) (bool, error) { return true, nil })

	restarted, failed := []string{}, []string{}
	run := func() {
		t.Helper()
		err := restartOneSystemdGatewayUnit(context.Background(), io.Discard, "icebath-gateway", "user", []string{"systemctl", "--user"}, 5.0,
			map[string][]string{}, &restarted, &failed, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	run()
	if len(drained) != 0 || len(writeVerbs) != 0 || len(restarted) != 0 || len(failed) != 0 {
		t.Fatalf("foreign unit touched: drained=%v verbs=%v restarted=%v failed=%v", drained, writeVerbs, restarted, failed)
	}

	mainPID = 4343
	run()
	if !reflect.DeepEqual(drained, []int{4343}) || !reflect.DeepEqual(restarted, []string{"icebath-gateway"}) || len(failed) != 0 {
		t.Fatalf("own unit: drained=%v restarted=%v failed=%v", drained, restarted, failed)
	}
}

func TestManualGatewayOfAnotherHomeIsNotStopped(t *testing.T) {
	own := fleetOwnHome(t)
	fleetPidHomes(t, map[int]string{111: own, 222: fleetForeignHome, 333: ""})
	vsSwap(t, &fleetGetServicePIDs, func(context.Context, bool) ([]int, error) { return nil, nil })
	vsSwap(t, &fleetFindGatewayPIDs, func(context.Context, map[int]bool, bool) ([]int, error) { return []int{111, 222, 333}, nil })
	vsSwap(t, &fleetFindProfileProcesses, func(context.Context, map[int]bool) ([]gatewaycli.ProfileGatewayProcess, error) { return nil, nil })
	vsSwap(t, &fleetWaitForGatewayExit, func(float64) bool { return true })
	type kill struct {
		pid   int
		force bool
	}
	killed := []kill{}
	vsSwap(t, &fleetTerminatePid, func(pid int, force bool, _ any) error {
		killed = append(killed, kill{pid, force})
		return nil
	})

	out := newGatewayRestartOutcome()
	if err := restartManualGateways(context.Background(), io.Discard, out, 5.0); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(killed, []kill{{111, false}}) {
		t.Fatalf("killed = %v", killed)
	}
	if !reflect.DeepEqual(out.KilledPIDs, map[int]bool{111: true}) {
		t.Fatalf("killed pids = %v", out.KilledPIDs)
	}
}

const (
	fleetOldPID   = 500
	fleetNewPID   = 626640
	fleetOtherPID = 777
)

// Runs the restart phase with one timed out unit and then verification, returning the output, receipt and marker clears
func updateWithFailedUnit(t *testing.T, mainPID int, fleetRows []*pyjson.Object, dashboardsDown map[int]bool) (string, map[string]any, int) {
	t.Helper()
	h := newFleetHarness(t)
	ctx := updaterecord.UpdateReceiptScope(h.ctx)
	var out bytes.Buffer
	vsSwap(t, &fleetFindGatewayPIDs, func(context.Context, map[int]bool, bool) ([]int, error) { return []int{fleetOldPID}, nil })
	vsSwap(t, &fleetScopedManualPids, func(_ context.Context, _ io.Writer, pids []int, _ map[int]bool, _ bool) ([]int, error) {
		return append([]int{}, pids...), nil
	})
	vsSwap(t, &fleetDrainBudget, func(context.Context) float64 { return 1.0 })
	vsSwap(t, &fleetRestartSystemdUnits, func(_ context.Context, _ io.Writer, _, failed *[]string, _ map[string]bool, _ float64, _ map[int]bool) error {
		*failed = append(*failed, "icebath-gateway")
		return nil
	})
	vsSwap(t, &fleetRestartManual, func(context.Context, io.Writer, *gatewayRestartOutcome, float64) error { return nil })
	vsSwap(t, &fleetForceKillStuck, func(context.Context, io.Writer, map[int]bool) {})
	vsSwap(t, &fleetRestartIdentitySHA, func(context.Context) string { return "new" })
	vsSwap(t, &fleetIsMacOS, func() bool { return false })
	vsSwap(t, &fleetSystemctl, func(_ context.Context, cmd []string, _ float64) (*pysubprocess.Completed, error) {
		if slices.Contains(cmd, "--user") {
			return completedText(cmd, "LoadState=loaded\nMainPID="+strconv.Itoa(mainPID)+"\n"), nil
		}
		return completedText(cmd, "LoadState=not-found\nMainPID=0\n"), nil
	})
	vsSwap(t, &fleetCollectVersionsWith, func([]int, []int) []*pyjson.Object {
		rows := make([]*pyjson.Object, len(fleetRows))
		for i, row := range fleetRows {
			rows[i] = pyjson.Clone(row).(*pyjson.Object)
			rows[i].Set("code_sha", "new")
			rows[i].Set("code_version", "1")
			rows[i].Set("state", "current")
		}
		return rows
	})
	installFleetClock(t)
	vsSwap(t, &fleetPrintLegacyUnits, func(context.Context, io.Writer) {})
	vsSwap(t, &fleetRefreshDashboard, func(context.Context, io.Writer, map[string]bool) map[int]bool { return dashboardsDown })
	vsSwap(t, &fleetSurvivingServeRuntimes, func(context.Context, *UpdatePlan) ([]any, error) { return []any{}, nil })
	vsSwap(t, &fleetAutoMigrate, func(context.Context, io.Writer) error { return nil })
	cleared := 0
	vsSwap(t, &fleetClearMarker, func(context.Context) { cleared++ })

	pid := fleetOldPID
	plan := NewUpdatePlan()
	plan.Runtimes = []*RuntimeRecord{{Kind: "gateway", Profile: "default", PID: &pid, Supervisor: "unknown", RestartVia: "systemd"}}
	updaterecord.BeginUpdateReceipt(ctx, nil, "")
	restart, err := restartGatewayFleetAfterUpdate(ctx, &out, plan, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyFleetAfterUpdate(ctx, &out, restart, plan, nil, true); err != nil {
		t.Fatal(err)
	}
	return out.String(), readLatestReceipt(t, h.home), cleared
}

func TestFailedUnitSettlesOnlyOnItsOwnFreshCurrentGateway(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd restart phase")
	}
	cases := []struct {
		name    string
		mainPID int
		rows    []*pyjson.Object
		settled bool
	}{
		{"own-fresh-mainpid", fleetNewPID, []*pyjson.Object{fleetObject("profile", "default", "pid", int64(fleetNewPID))}, true},
		{"other-profile-row", 0, []*pyjson.Object{fleetObject("profile", "coder", "pid", int64(fleetOtherPID))}, false},
		{"pre-restart-pid", fleetOldPID, []*pyjson.Object{fleetObject("profile", "default", "pid", int64(fleetOldPID))}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, receipt, cleared := updateWithFailedUnit(t, tc.mainPID, tc.rows, nil)
			followups := followupSteps(receipt)
			restart, _ := receipt["gateway_restart"].(map[string]any)
			if tc.settled {
				if strings.Contains(out, "Update incomplete") || len(followups) != 0 || restart["incomplete"] != false ||
					len(restart["failed_units"].([]any)) != 0 || cleared != 1 {
					t.Fatalf("out=%q followups=%v restart=%v cleared=%d", out, followups, restart, cleared)
				}
				return
			}
			if !strings.Contains(out, "Update incomplete — some units were not restarted") || !strings.Contains(out, "    - icebath-gateway") ||
				!reflect.DeepEqual(followups, []string{"gateway_restart"}) || restart["incomplete"] != true || cleared != 0 {
				t.Fatalf("out=%q followups=%v restart=%v cleared=%d", out, followups, restart, cleared)
			}
		})
	}
}

func TestSettledUnitKeepsTheReceiptIncompleteWhileADashboardIsDown(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd restart phase")
	}
	out, receipt, cleared := updateWithFailedUnit(t, fleetNewPID,
		[]*pyjson.Object{fleetObject("profile", "default", "pid", int64(fleetNewPID))}, map[int]bool{4242: true})
	restart, _ := receipt["gateway_restart"].(map[string]any)
	if strings.Contains(out, "Update incomplete — some units were not restarted") ||
		!reflect.DeepEqual(followupSteps(receipt), []string{"gateway_restart"}) ||
		len(restart["failed_units"].([]any)) != 0 || restart["incomplete"] != true || cleared != 0 {
		t.Fatalf("out=%q receipt=%v cleared=%d", out, receipt, cleared)
	}
}

func fleetSnapshots(t *testing.T, snapshots ...[]*pyjson.Object) {
	t.Helper()
	next := 0
	vsSwap(t, &fleetCollectVersionsWith, func([]int, []int) []*pyjson.Object {
		if next >= len(snapshots) {
			t.Fatal("fleet probe polled past the scripted snapshots")
		}
		next++
		return snapshots[next-1]
	})
}

func TestSnapshotWaitsForLateCurrentGatewayState(t *testing.T) {
	clock := installFleetClock(t)
	expected := fleetObject("profile", "default", "pid", int64(202), "code_sha", "new", "state", "current")
	snapshots := make([][]*pyjson.Object, 20)
	snapshots[19] = []*pyjson.Object{expected}
	fleetSnapshots(t, snapshots...)

	result, err := collectFleetSnapshot(context.Background(), &gatewayRestartOutcome{PreRestartGatewayPIDs: []int{101}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0] != expected {
		t.Fatalf("result = %s", fleetJSON(t, fleetRowsAsAny(result)))
	}
	if clock.now.Seconds() <= 30.0 || clock.now.Seconds() > fleetProbeSettleTimeoutSeconds {
		t.Fatalf("waited %v", clock.now)
	}
}

func TestSnapshotStopsWaitingOnceTheRestartedUnitIsDead(t *testing.T) {
	clock := installFleetClock(t)
	vsSwap(t, &fleetCollectVersionsWith, func([]int, []int) []*pyjson.Object { return []*pyjson.Object{} })
	vsSwap(t, &fleetSystemctl, func(_ context.Context, cmd []string, _ float64) (*pysubprocess.Completed, error) {
		return completedText(cmd, "LoadState=loaded\nActiveState=failed\n"), nil
	})
	restart := &gatewayRestartOutcome{PreRestartGatewayPIDs: []int{101}, RestartedScopedUnits: map[string]bool{"user/icebath-gateway.service": true}}
	result, err := collectFleetSnapshot(context.Background(), restart, true)
	if err != nil || len(result) != 0 {
		t.Fatalf("result = %v, %v", result, err)
	}
	if clock.now.Seconds() >= 30.0 {
		t.Fatalf("waited %v", clock.now)
	}
}

func TestSnapshotKeepsWaitingWhenTheUnitIsUnknownToTheAskedScope(t *testing.T) {
	clock := installFleetClock(t)
	vsSwap(t, &fleetCollectVersionsWith, func([]int, []int) []*pyjson.Object { return []*pyjson.Object{} })
	vsSwap(t, &fleetSystemctl, func(_ context.Context, cmd []string, _ float64) (*pysubprocess.Completed, error) {
		if slices.Contains(cmd, "--user") {
			return completedText(cmd, "LoadState=loaded\nActiveState=active\n"), nil
		}
		return completedText(cmd, "LoadState=not-found\nActiveState=inactive\n"), nil
	})
	units := map[string]bool{"system/icebath-gateway.service": true}
	if gone, err := restartedUnitsGone(context.Background(), units); err != nil || gone {
		t.Fatalf("gone = %v, %v", gone, err)
	}
	restart := &gatewayRestartOutcome{PreRestartGatewayPIDs: []int{101}, RestartedScopedUnits: units}
	result, err := collectFleetSnapshot(context.Background(), restart, true)
	if err != nil || len(result) != 0 {
		t.Fatalf("result = %v, %v", result, err)
	}
	if clock.now.Seconds() < fleetProbeSettleTimeoutSeconds {
		t.Fatalf("waited %v", clock.now)
	}
}

func TestSnapshotWaitsForTheRelaunchedPidToPublishItsIdentity(t *testing.T) {
	clock := installFleetClock(t)
	unknown := func() *pyjson.Object {
		return fleetObject("profile", "default", "pid", int64(34516), "code_sha", nil, "code_version", nil, "state", "unknown")
	}
	current := fleetObject("profile", "default", "pid", int64(34516), "code_sha", "new", "code_version", "0.21.3", "state", "current")
	booting := []*pyjson.Object{unknown()}
	fleetSnapshots(t, booting, booting, booting, booting, booting, []*pyjson.Object{current})
	restart := &gatewayRestartOutcome{PreRestartGatewayPIDs: []int{32512}, RestartedScopedUnits: map[string]bool{}}

	result, err := collectFleetSnapshot(context.Background(), restart, true)
	if err != nil || len(result) != 1 || result[0] != current {
		t.Fatalf("result = %v, %v", result, err)
	}
	if clock.now != 12*time.Second {
		t.Fatalf("waited %v", clock.now)
	}

	clock.now = 0
	vsSwap(t, &fleetCollectVersionsWith, func([]int, []int) []*pyjson.Object { return []*pyjson.Object{unknown()} })
	survivor := &gatewayRestartOutcome{PreRestartGatewayPIDs: []int{34516}, RestartedScopedUnits: map[string]bool{}}
	result, err = collectFleetSnapshot(context.Background(), survivor, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, flagged := result[0].Get("identity_pending"); clock.now != 2*time.Second || flagged {
		t.Fatalf("survivor waited %v flagged %v", clock.now, flagged)
	}

	clock.now = 0
	result, err = collectFleetSnapshot(context.Background(), restart, true)
	if err != nil {
		t.Fatal(err)
	}
	if flagged, _ := result[0].Get("identity_pending"); clock.now.Seconds() < fleetProbeSettleTimeoutSeconds || flagged != true {
		t.Fatalf("successor waited %v flagged %v", clock.now, flagged)
	}
}

func TestFleetCompletionPreservesRuntimeVerdictAndRestartObligation(t *testing.T) {
	cases := []struct {
		name           string
		updateComplete bool
		state          any
	}{{"complete-current", true, "current"}, {"owed-current", false, "current"}, {"stale", true, "stale"}, {"down", true, "down"}, {"no-rows", true, nil}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFleetHarness(t)
			ctx := updaterecord.UpdateReceiptScope(h.ctx)
			refreshed := []map[string]bool{}
			migrated := 0
			snapshot := []*pyjson.Object{}
			if tc.state != nil {
				snapshot = append(snapshot, fleetObject("profile", "default", "pid", int64(1234), "state", tc.state))
			}
			restart := newGatewayRestartOutcome()
			restart.PreRestartGatewayPIDs = []int{1234}
			restart.RestartedServices = []string{"icebath-gateway"}
			vsSwap(t, &fleetPrintLegacyUnits, func(context.Context, io.Writer) {})
			vsSwap(t, &fleetSurvivingServeRuntimes, func(context.Context, *UpdatePlan) ([]any, error) { return []any{}, nil })
			vsSwap(t, &fleetRefreshDashboard, func(_ context.Context, _ io.Writer, already map[string]bool) map[int]bool {
				refreshed = append(refreshed, already)
				return nil
			})
			vsSwap(t, &fleetAutoMigrate, func(context.Context, io.Writer) error { migrated++; return nil })
			// The drain signal is withheld so a live pid 1234 of the test host is never signalled
			vsSwap(t, &fleetGracefulRestart, func(int, float64, func()) bool { return false })
			vsSwap(t, &fleetCollectSnapshot, func(_ context.Context, outcome *gatewayRestartOutcome, rowsExpected bool) ([]*pyjson.Object, error) {
				if outcome != restart || !rowsExpected {
					t.Fatalf("collect called with %p %v", outcome, rowsExpected)
				}
				return snapshot, nil
			})
			h.arm("", nil)
			if !h.armed() {
				t.Fatal("obligation did not arm")
			}
			healthy := tc.updateComplete && tc.state == "current"
			updaterecord.BeginUpdateReceipt(ctx, nil, "")
			if err := verifyFleetAfterUpdate(ctx, io.Discard, restart, nil, nil, tc.updateComplete); err != nil {
				t.Fatal(err)
			}

			receipt := readLatestReceipt(t, h.home)
			wantFollowups := []string{"gateway_restart"}
			if tc.state == "current" {
				wantFollowups = []string{}
			}
			if receipt["outcome"] != "success" || !reflect.DeepEqual(followupSteps(receipt), wantFollowups) {
				t.Fatalf("receipt outcome %v followups %v", receipt["outcome"], followupSteps(receipt))
			}
			var wantFleet any
			if err := json.Unmarshal([]byte(fleetJSON(t, fleetRowsAsAny(snapshot))), &wantFleet); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(receipt["fleet"], wantFleet) {
				t.Fatalf("fleet = %v, want %v", receipt["fleet"], wantFleet)
			}
			if restart.Incomplete != (tc.state != "current") || h.armed() != (tc.state != "current") {
				t.Fatalf("incomplete %v armed %v", restart.Incomplete, h.armed())
			}
			if wantMigrated := map[bool]int{true: 1, false: 0}[healthy]; migrated != wantMigrated {
				t.Fatalf("migrated = %d", migrated)
			}
			if !reflect.DeepEqual(refreshed, []map[string]bool{{"icebath-gateway": true}}) {
				t.Fatalf("refreshed = %v", refreshed)
			}
		})
	}
}

func TestVerifyFleetHandsStaleRowsToSurvivorSignalling(t *testing.T) {
	h := newFleetHarness(t)
	staleFleet := []*pyjson.Object{fleetObject("profile", "default", "pid", int64(4242), "state", "stale", "code_sha", strings.Repeat("a", 40))}
	var seen []any
	vsSwap(t, &fleetPrintLegacyUnits, func(context.Context, io.Writer) {})
	vsSwap(t, &fleetRefreshDashboard, func(context.Context, io.Writer, map[string]bool) map[int]bool { return map[int]bool{} })
	vsSwap(t, &fleetSurvivingServeRuntimes, func(context.Context, *UpdatePlan) ([]any, error) { return []any{}, nil })
	vsSwap(t, &fleetCollectSnapshot, func(context.Context, *gatewayRestartOutcome, bool) ([]*pyjson.Object, error) { return staleFleet, nil })
	vsSwap(t, &fleetSignalStaleSurvivors, func(_ context.Context, _ io.Writer, fleet []any, _ *gatewayRestartOutcome, _ float64) []int {
		if seen == nil {
			seen = fleet
		}
		return []int{}
	})
	restart := newGatewayRestartOutcome()
	restart.PreRestartGatewayPIDs = []int{4242}
	if err := verifyFleetAfterUpdate(h.ctx, io.Discard, restart, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if !restart.Incomplete {
		t.Fatal("a stale fleet left the restart complete")
	}
	if len(seen) != 1 || seen[0] != any(staleFleet[0]) {
		t.Fatalf("survivor pass saw %v", seen)
	}
}

func secondsDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}
