//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Points both fleet probes at the rows a test keeps editing
func pinFleetRows(t *testing.T, rows *[]*pyjson.Object) {
	t.Helper()
	read := func() []*pyjson.Object { return append([]*pyjson.Object{}, (*rows)...) }
	vsSwap(t, &fleetCollectVersions, read)
	vsSwap(t, &fleetCollectVersionsWith, func([]int, []int) []*pyjson.Object { return read() })
}

// Fakes the legacy unit check of the verification pass
func quietVerification(t *testing.T) {
	t.Helper()
	vsSwap(t, &fleetPrintLegacyUnits, func(context.Context, io.Writer) {})
}

func unmappedStopOutcome(pids ...int) *gatewayRestartOutcome {
	out := newGatewayRestartOutcome()
	out.PreRestartGatewayPIDs = append([]int{}, pids...)
	for _, pid := range pids {
		out.KilledPIDs[pid] = true
		out.StoppedUnmappedPIDs[pid] = true
	}
	return out
}

func gatewayRuntime(profile string, pid int) *RuntimeRecord {
	record := &RuntimeRecord{Kind: "gateway", Profile: profile, Supervisor: "manual"}
	if pid != 0 {
		record.PID = &pid
	}
	return record
}

func owesFleetRestart(t *testing.T, h *fleetHarness) bool {
	t.Helper()
	owed, err := updateOwesFleetRestart(h.ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	return owed
}

func TestCallSiteWiringEmptyProbeSettlesAndFailsOnlyWhenRowsExpected(t *testing.T) {
	for _, hadGateway := range []bool{false, true} {
		name := map[bool]string{false: "idle", true: "plan-saw-gateway"}[hadGateway]
		t.Run(name, func(t *testing.T) {
			h := newFleetHarness(t)
			ctx := updaterecord.UpdateReceiptScope(h.ctx)
			quietVerification(t)
			vsSwap(t, &fleetReportUnaccounted, func(io.Writer, []*pyjson.Object) bool { return false })
			vsSwap(t, &fleetAutoMigrate, func(context.Context, io.Writer) error { return nil })
			events := []string{}
			base, now := time.Unix(1_700_000_000, 0), time.Duration(0)
			vsSwap(t, &fleetMonotonic, func() time.Time { return base.Add(now) })
			vsSwap(t, &fleetSleep, func(d time.Duration) {
				events = append(events, "settle")
				now += d
			})
			vsSwap(t, &fleetCollectVersionsWith, func([]int, []int) []*pyjson.Object {
				events = append(events, "probe")
				return []*pyjson.Object{}
			})
			restart := newGatewayRestartOutcome()
			plan := NewUpdatePlan()
			if hadGateway {
				plan.Runtimes = []*RuntimeRecord{gatewayRuntime("default", 0)}
			}
			updaterecord.BeginUpdateReceipt(ctx, nil, "")
			cleared := 0
			vsSwap(t, &fleetClearMarker, func(context.Context) { cleared++ })
			var out bytes.Buffer
			if err := verifyFleetAfterUpdate(ctx, &out, restart, plan, nil, true); err != nil {
				t.Fatal(err)
			}
			if hadGateway {
				probes := 0
				for _, event := range events {
					if event == "probe" {
						probes++
					}
				}
				if events[0] != "settle" || probes <= 1 || !strings.Contains(out.String(), "returned no rows") {
					t.Fatalf("events=%v out=%q", events, out.String())
				}
			} else if !reflect.DeepEqual(events, []string{"probe"}) {
				t.Fatalf("events = %v", events)
			}
			if restart.Incomplete != hadGateway || cleared != map[bool]int{false: 1, true: 0}[hadGateway] {
				t.Fatalf("incomplete=%v cleared=%d", restart.Incomplete, cleared)
			}
			receipt := readLatestReceipt(t, h.home)
			wantFollowups := []string{}
			if hadGateway {
				wantFollowups = []string{"gateway_restart"}
			}
			if receipt["outcome"] != "success" || !reflect.DeepEqual(followupSteps(receipt), wantFollowups) {
				t.Fatalf("outcome=%v followups=%v", receipt["outcome"], followupSteps(receipt))
			}
		})
	}
}

func TestUnmappedStopsAreNotExpectedRows(t *testing.T) {
	out := unmappedStopOutcome(101, 102)
	pre, killed := out.fleetProbeSignals()
	if fleetProbeExpectedRuntimes(NewUpdatePlan(), pre, nil, out.RestartedServices, killed) {
		t.Fatal("unmapped stops counted as expected rows")
	}
	delete(out.StoppedUnmappedPIDs, 102)
	pre, killed = out.fleetProbeSignals()
	if !fleetProbeExpectedRuntimes(NewUpdatePlan(), pre, nil, out.RestartedServices, killed) {
		t.Fatal("a mapped stop predicted no row")
	}
}

func TestUnmappedStopKeepsTheRestartOwed(t *testing.T) {
	h := newFleetHarness(t)
	ctx := updaterecord.UpdateReceiptScope(h.ctx)
	quietVerification(t)
	vsSwap(t, &fleetCollectVersionsWith, func([]int, []int) []*pyjson.Object { return []*pyjson.Object{} })
	cleared := 0
	vsSwap(t, &fleetClearMarker, func(context.Context) { cleared++ })
	out := unmappedStopOutcome(101)
	updaterecord.BeginUpdateReceipt(ctx, nil, "")
	if err := verifyFleetAfterUpdate(ctx, io.Discard, out, NewUpdatePlan(), nil, true); err != nil {
		t.Fatal(err)
	}
	receipt := readLatestReceipt(t, h.home)
	if receipt["outcome"] != "success" || !reflect.DeepEqual(followupSteps(receipt), []string{"gateway_restart"}) {
		t.Fatalf("receipt = %v", receipt)
	}
	reason, _ := receipt["followups"].([]any)[0].(map[string]any)["reason"].(string)
	if !strings.Contains(reason, "101") || cleared != 0 {
		t.Fatalf("reason=%q cleared=%d", reason, cleared)
	}
}

func TestUnmappedStopDebtSurvivesStartupUntilACurrentGatewayRuns(t *testing.T) {
	h := newFleetHarness(t)
	ctx := updaterecord.UpdateReceiptScope(h.ctx)
	h.checkout("head")
	quietVerification(t)
	vsSwap(t, &fleetHostOwesNoRestart, func(context.Context) bool { return true })
	rows := []*pyjson.Object{}
	pinFleetRows(t, &rows)
	h.arm("head", nil)
	out := unmappedStopOutcome(101)
	updaterecord.BeginUpdateReceipt(ctx, nil, "")
	if err := verifyFleetAfterUpdate(ctx, io.Discard, out, NewUpdatePlan(), nil, true); err != nil {
		t.Fatal(err)
	}

	if !owesFleetRestart(t, h) {
		t.Fatal("the unmapped stop was not owed")
	}
	needed, err := pendingFleetRestartNeeded(h.ctx, map[string]any{})
	if err != nil || !needed || !h.armed() {
		t.Fatalf("needed=%v armed=%v err=%v", needed, h.armed(), err)
	}

	rows = append(rows, fleetObject("profile", "default", "state", "current", "code_sha", "head"))
	if owesFleetRestart(t, h) || h.armed() {
		t.Fatalf("a current gateway left the debt armed=%v", h.armed())
	}
}

func TestUnmappedStopDebtIsNotSettledByTheMappedGatewaysRestart(t *testing.T) {
	h := newFleetHarness(t)
	h.checkout("head")
	h.arm("head", []any{
		map[string]any{"kind": "gateway", "profile": "default", "pid": int64(7)},
		map[string]any{"kind": "gateway", "profile": nil, "pid": int64(101), "stopped_unmapped": true},
	})
	rows := []*pyjson.Object{fleetObject("profile", "default", "state", "current", "code_sha", "head", "pid", int64(8))}
	pinFleetRows(t, &rows)

	if !owesFleetRestart(t, h) || !h.armed() {
		t.Fatalf("the mapped restart settled the unmapped debt, armed=%v", h.armed())
	}

	rows = append(rows, fleetObject("profile", "work", "state", "current", "code_sha", "head", "pid", int64(9)))
	if owesFleetRestart(t, h) || h.armed() {
		t.Fatalf("a second current gateway left the debt armed=%v", h.armed())
	}
}

func TestSameShaRetryAddsNewlyStoppedUnmappedDebtToAnExistingInventory(t *testing.T) {
	h := newFleetHarness(t)
	ctx := updaterecord.UpdateReceiptScope(h.ctx)
	h.checkout("head")
	quietVerification(t)
	vsSwap(t, &fleetProbeSettleTimeoutSeconds, 0)
	vsSwap(t, &fleetSignalStaleSurvivors, func(context.Context, io.Writer, []any, *gatewayRestartOutcome, float64) []int { return nil })
	rows := []*pyjson.Object{fleetObject("profile", "default", "state", "down", "code_sha", "old", "pid", int64(7))}
	pinFleetRows(t, &rows)
	h.arm("head", []any{map[string]any{"kind": "gateway", "profile": "default", "pid": int64(7)}})
	out := unmappedStopOutcome(7999)
	updaterecord.BeginUpdateReceipt(ctx, nil, "")
	plan := NewUpdatePlan()
	plan.Runtimes = []*RuntimeRecord{gatewayRuntime("default", 7)}
	if err := verifyFleetAfterUpdate(ctx, io.Discard, out, plan, nil, true); err != nil {
		t.Fatal(err)
	}

	fields, readable := fleetObligationFields(h.ctx)
	if !readable {
		t.Fatal("the obligation is unreadable")
	}
	var inventory struct {
		Runtimes []map[string]any `json:"runtimes"`
	}
	if err := json.Unmarshal([]byte(fields["inventory"]), &inventory); err != nil {
		t.Fatal(err)
	}
	holds := func(want map[string]any) bool {
		for _, row := range inventory.Runtimes {
			if reflect.DeepEqual(row, want) {
				return true
			}
		}
		return false
	}
	if !holds(map[string]any{"kind": "gateway", "profile": "default", "pid": float64(7)}) ||
		!holds(map[string]any{"kind": "gateway", "profile": nil, "pid": float64(7999), "stopped_unmapped": true}) {
		t.Fatalf("inventory = %v", inventory.Runtimes)
	}
	rows = []*pyjson.Object{fleetObject("profile", "default", "state", "current", "code_sha", "head", "pid", int64(8))}
	if obsolete, err := markerOnlyRestartObsolete(h.ctx); err != nil || obsolete || !h.armed() {
		t.Fatalf("obsolete=%v armed=%v err=%v", obsolete, h.armed(), err)
	}
	rows = append(rows, fleetObject("profile", "work", "state", "current", "code_sha", "head", "pid", int64(9)))
	if obsolete, err := markerOnlyRestartObsolete(h.ctx); err != nil || !obsolete {
		t.Fatalf("obsolete=%v err=%v", obsolete, err)
	}
}

func TestCurrentSuccessorsSettleHistoricalObligations(t *testing.T) {
	for _, profiles := range [][]string{{"alpha"}, {"alpha", "beta"}} {
		t.Run(strings.Join(profiles, "-"), func(t *testing.T) {
			h := newFleetHarness(t)
			runtimes, rows := []any{}, []map[string]any{}
			for _, profile := range profiles {
				runtimes = append(runtimes, map[string]any{"kind": "gateway", "profile": profile, "pid": 1, "code_sha": "old"})
				rows = append(rows, map[string]any{"profile": profile, "pid": int64(2), "state": "current", "code_sha": "new"})
			}
			h.receipt(map[string]any{"outcome": "failed", "plan": map[string]any{"runtimes": runtimes}})
			path := filepath.Join(h.home, "logs", "update_receipts", "latest.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			h.checkout("new")
			h.fleet(rows...)
			if h.pendingNeeded() {
				t.Fatal("current successors left the historical obligation pending")
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("the historical receipt changed: %v", err)
			}
		})
	}
}

func TestEveryOwedIdentityRequiresCurrentEvidence(t *testing.T) {
	for _, bad := range []string{"missing", "unknown", "down", "stale", "wrong-sha", "wrong-kind", "unknown-profile", "marker"} {
		t.Run(bad, func(t *testing.T) {
			h := newFleetHarness(t)
			if bad == "marker" {
				inventory := `{"version": 1, "runtimes": [{"kind": "gateway", "profile": "beta"}]}`
				if err := os.WriteFile(filepath.Join(h.home, "fleet_restart_pending"), []byte("expected_sha=future\ninventory="+inventory+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			owed := map[string]any{"kind": "gateway", "profile": "beta", "code_sha": "old"}
			if bad == "wrong-kind" {
				owed["kind"] = "serve"
			}
			if bad == "unknown-profile" {
				owed["profile"] = "unknown"
			}
			h.receipt(map[string]any{"outcome": "failed", "plan": map[string]any{"runtimes": []any{
				map[string]any{"kind": "gateway", "profile": "alpha", "code_sha": "old"}, owed}}})
			rows := []map[string]any{{"profile": "alpha", "state": "current", "code_sha": "new"}}
			if bad != "missing" {
				state, sha := "current", "new"
				if bad == "unknown" || bad == "down" || bad == "stale" {
					state = bad
				}
				if bad == "wrong-sha" {
					sha = "old"
				}
				rows = append(rows, map[string]any{"profile": "beta", "state": state, "code_sha": sha})
			}
			h.checkout("new")
			h.fleet(rows...)
			if !h.pendingNeeded() {
				t.Fatal("an owed identity without current evidence was settled")
			}
		})
	}
}

func TestCompletedRestartOfAPlannedGatewayDischargesTheReceipt(t *testing.T) {
	h := newFleetHarness(t)
	h.receipt(map[string]any{
		"outcome":         "failed",
		"post_update":     map[string]any{"sha": "pulled"},
		"gateway_restart": map[string]any{"restarted_services": []any{"icebath-gateway"}, "incomplete": false},
		"plan": map[string]any{"runtimes": []any{
			map[string]any{"kind": "gateway", "profile": "default", "code_sha": "old", "supervisor": "manual"},
		}},
	})
	h.checkout("new")
	h.fleet(map[string]any{"profile": "default", "state": "current", "code_sha": "new"})
	owed, err := updateOwesFleetRestart(h.ctx, nil)
	if err != nil || owed {
		t.Fatalf("owed=%v err=%v", owed, err)
	}
	if h.pendingNeeded() {
		t.Fatal("a current gateway kept the restart pending")
	}
}

// Runs verification over one fleet row in the given state with the restart phase's outcome
func verifyRestartCase(t *testing.T, restart *gatewayRestartOutcome, fleetState string) (*fleetHarness, map[string]any, string) {
	t.Helper()
	h := newFleetHarness(t)
	ctx := updaterecord.UpdateReceiptScope(h.ctx)
	vsSwap(t, &fleetPrintLegacyUnits, func(context.Context, io.Writer) {})
	vsSwap(t, &fleetSystemctl, func(_ context.Context, cmd []string, _ float64) (*pysubprocess.Completed, error) {
		return completedText(cmd, "LoadState=not-found\nMainPID=0\n"), nil
	})
	vsSwap(t, &fleetAutoMigrate, func(context.Context, io.Writer) error { return nil })
	vsSwap(t, &fleetSignalStaleSurvivors, func(context.Context, io.Writer, []any, *gatewayRestartOutcome, float64) []int { return nil })
	vsSwap(t, &fleetCollectSnapshot, func(context.Context, *gatewayRestartOutcome, bool) ([]*pyjson.Object, error) {
		return []*pyjson.Object{fleetObject("profile", "default", "pid", int64(1563620), "state", fleetState)}, nil
	})
	h.arm("", nil)
	updaterecord.BeginUpdateReceipt(ctx, nil, "")
	var out bytes.Buffer
	if err := verifyFleetAfterUpdate(ctx, &out, restart, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	return h, readLatestReceipt(t, h.home), out.String()
}

func declinedGatewayUnitRestart() *gatewayRestartOutcome {
	restart := newGatewayRestartOutcome()
	restart.Incomplete = true
	restart.PreRestartGatewayPIDs = []int{1234}
	restart.RestartedServices = []string{"icebath-gateway"}
	restart.FailedOrStaleUnits = []string{"icebath-gateway-work"}
	return restart
}

func firstFollowupReason(t *testing.T, receipt map[string]any) string {
	t.Helper()
	rows, _ := receipt["followups"].([]any)
	if len(rows) == 0 {
		t.Fatalf("no follow-up recorded: %v", receipt)
	}
	reason, _ := rows[0].(map[string]any)["reason"].(string)
	return reason
}

func TestGatewayDebtNamesGatewayRestart(t *testing.T) {
	for _, name := range []string{"declined-unit", "stale-gateway", "phase-error"} {
		t.Run(name, func(t *testing.T) {
			restart, state := declinedGatewayUnitRestart(), "current"
			switch name {
			case "stale-gateway":
				state = "stale"
			case "phase-error":
				restart.PhaseErrors = append(restart.PhaseErrors, "boom")
			}
			h, receipt, _ := verifyRestartCase(t, restart, state)
			if !reflect.DeepEqual(followupSteps(receipt), []string{"gateway_restart"}) {
				t.Fatalf("followups = %v", followupSteps(receipt))
			}
			reason := firstFollowupReason(t, receipt)
			if reason != gatewayOwedReason+gatewayRecovery {
				t.Fatalf("reason = %q", reason)
			}
			if !h.armed() {
				t.Fatal("the obligation was cleared")
			}
		})
	}
}

func TestGatewayDebtNamesTheUnmappedStops(t *testing.T) {
	if got, want := owedRestartReason([]int{11, 7}), gatewayOwedReason+" (stopped PIDs 11, 7)"+gatewayRecovery; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}

func TestIncompleteRunNeverSettlesTheHostObligationMidVerification(t *testing.T) {
	calls := 0
	vsSwap(t, &fleetNamedStillOwed, func(context.Context) (bool, error) { calls++; return false, nil })
	h, _, _ := verifyRestartCase(t, declinedGatewayUnitRestart(), "current")
	if calls != 0 || !h.armed() {
		t.Fatalf("owed check ran %d times, armed = %v", calls, h.armed())
	}
}
