//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	gatewayOwedReason = "gateways may still run pre-update code or were stopped without a successor"
	gatewayRecovery   = "; recover with `icebath gateway restart`"
)

// Seconds the fleet probe keeps polling for a restarted gateway to publish its state
var fleetProbeSettleTimeoutSeconds = 120.0

// Calls of the verification pass the tests replace
var (
	fleetCollectSnapshot      = collectFleetSnapshot
	fleetProbeExpected        = fleetProbeExpectedRuntimes
	fleetPrintMatrix          = printFleetVersionMatrixTo
	fleetSignalStaleSurvivors = signalStaleFleetSurvivors
	fleetMatchOutcomes        = matchRuntimeOutcomes
	fleetReportUnaccounted    = reportUnaccountedRuntimes
	fleetRecordFollowup       = updaterecord.RecordFollowup
	fleetFinalizeReceipt      = updaterecord.FinalizeUpdateReceipt
	fleetAutoMigrate          = gatewayrun.MaybeAutoMigrateAfterUpdate
	fleetNamedStillOwed       = namedGatewaysStillOwed
)

// Prints the post update fleet matrix to w, true when a gateway is provably stale or down
func printFleetVersionMatrixTo(w io.Writer, fleet []*jsonutil.Object) bool {
	saved := fleetStdout
	fleetStdout = w
	defer func() { fleetStdout = saved }()
	return PrintFleetVersionMatrix(fleet)
}

// Fleet version rows, polled over a bounded settle window when runtimes are expected
func collectFleetSnapshot(ctx context.Context, restart *gatewayRestartOutcome, rowsExpected bool) ([]*jsonutil.Object, error) {
	pending := sortedPIDs(restart.SelfRestartPendingPIDs)
	if len(pending) == 0 {
		pending = nil
	}
	prePIDs := restart.PreRestartGatewayPIDs
	if !rowsExpected {
		return fleetCollectVersionsWith(prePIDs, pending), nil
	}
	deadline := fleetMonotonic().Add(time.Duration(fleetProbeSettleTimeoutSeconds * float64(time.Second)))
	for {
		fleetSleep(2 * time.Second)
		snapshot := fleetCollectVersionsWith(prePIDs, pending)
		var unstamped []*jsonutil.Object
		down := false
		for _, row := range snapshot {
			if fleetRowIdentityPending(row, prePIDs) {
				unstamped = append(unstamped, row)
			}
			down = down || anyutil.Equal(rowValue(row, "state"), "down")
		}
		if len(snapshot) > 0 && len(unstamped) == 0 && !down {
			return snapshot, nil
		}
		gone := false
		if fleetMonotonic().Before(deadline) {
			var err error
			if gone, err = restartedUnitsGone(ctx, restart.RestartedScopedUnits); err != nil {
				return nil, err
			}
		}
		if gone || !fleetMonotonic().Before(deadline) {
			for _, row := range unstamped {
				row.Set("identity_pending", true)
			}
			return snapshot, nil
		}
	}
}

// True for an unknown row without a sha from a pid that did not exist when the update started
func fleetRowIdentityPending(row any, preRestartPIDs []int) bool {
	if !anyutil.Equal(rowValue(row, "state"), "unknown") || anyutil.IsTrue(rowValue(row, "code_sha")) {
		return false
	}
	if preRestartPIDs == nil {
		return false
	}
	pid, ok := intValue(rowValue(row, "pid"))
	if !ok {
		return true
	}
	for _, pre := range preRestartPIDs {
		if int64(pre) == pid {
			return false
		}
	}
	return true
}

// Properties of a systemctl show answer keyed by name
func systemctlShowProps(stdout string) map[string]string {
	props := map[string]string{}
	for _, line := range textutil.Lines(stdout) {
		if key, value, found := strings.Cut(line, "="); found {
			props[key] = value
		}
	}
	return props
}

// Systemctl prefix of a scope name, false for a scope systemd does not have
func systemdScopeCmd(scope string) ([]string, bool) {
	for _, known := range systemdScopes {
		if known.Name == scope {
			return known.Cmd, true
		}
	}
	return nil, false
}

// True when every restarted unit is loaded in its scope and neither active, activating nor reloading
func restartedUnitsGone(ctx context.Context, scopedUnits map[string]bool) (bool, error) {
	if len(scopedUnits) == 0 {
		return false, nil
	}
	ordered := make([]string, 0, len(scopedUnits))
	for scoped := range scopedUnits {
		ordered = append(ordered, scoped)
	}
	sort.Strings(ordered)
	for _, scoped := range ordered {
		scope, name, _ := strings.Cut(scoped, "/")
		scopeCmd, known := systemdScopeCmd(scope)
		if !known {
			return false, nil
		}
		shown, err := systemctl(ctx, fleetArgv(scopeCmd, "show", "-p", "LoadState,ActiveState", name), 5)
		if err != nil {
			if isCommandMissing(err) || asCommandTimeout(err) != nil {
				return false, nil
			}
			return false, err
		}
		props := systemctlShowProps(completedStdout(shown))
		if props["LoadState"] != "loaded" {
			return false, nil
		}
		switch props["ActiveState"] {
		case "active", "activating", "reloading":
			return false, nil
		}
	}
	return true, nil
}

// Gateway pids alive after the restart phase per profile, from the fleet rows that are not down
func liveGatewayPidsFromFleet(fleetRows []*jsonutil.Object) map[string]map[int]bool {
	live := map[string]map[int]bool{}
	for _, row := range fleetRows {
		profile := rowValue(row, "profile")
		pid, isInt := intValue(rowValue(row, "pid"))
		if !anyutil.IsTrue(profile) || !isInt || anyutil.Equal(rowValue(row, "state"), "down") {
			continue
		}
		name := anyutil.String(profile)
		if live[name] == nil {
			live[name] = map[int]bool{}
		}
		live[name][int(pid)] = true
	}
	return live
}

// True when every scope that has the gateway unit loaded runs it as one of the pids
func gatewayUnitRunsOneOf(ctx context.Context, unit string, pids map[int]bool) (bool, error) {
	if len(pids) == 0 || !(unit == "icebath-gateway" || strings.HasPrefix(unit, "icebath-gateway-")) {
		return false, nil
	}
	loaded := false
	for _, scope := range systemdScopes {
		shown, err := systemctl(ctx, fleetArgv(scope.Cmd, "show", "-p", "LoadState,MainPID", unit), 5)
		if err != nil {
			if isCommandMissing(err) || asCommandTimeout(err) != nil || errorutil.IsSystem(err) {
				return false, nil
			}
			return false, err
		}
		props := systemctlShowProps(completedStdout(shown))
		if props["LoadState"] != "loaded" {
			continue
		}
		loaded = true
		mainPID := 0
		if text := props["MainPID"]; text != "" {
			parsed, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
			if err != nil {
				return false, nil
			}
			mainPID = int(parsed)
		}
		if !pids[mainPID] {
			return false, nil
		}
	}
	return loaded, nil
}

// Credits each failed gateway unit whose own main pid the fleet probe shows serving this checkout
func settleUnitsProvenCurrent(ctx context.Context, w io.Writer, restart *gatewayRestartOutcome, fleetRows []*jsonutil.Object) (bool, error) {
	pre := restart.PreRestartGatewayPIDs
	if pre == nil || len(restart.FailedOrStaleUnits) == 0 {
		return false, nil
	}
	fresh := map[int]bool{}
	for _, row := range fleetRows {
		if pid, ok := intValue(rowValue(row, "pid")); ok && anyutil.Equal(rowValue(row, "state"), "current") {
			fresh[int(pid)] = true
		}
	}
	for _, pid := range pre {
		delete(fresh, pid)
	}
	proven := map[string]bool{}
	provenOrder := []string{}
	checked := map[string]bool{}
	for _, unit := range restart.FailedOrStaleUnits {
		if checked[unit] {
			continue
		}
		checked[unit] = true
		runs, err := gatewayUnitRunsOneOf(ctx, unit, fresh)
		if err != nil {
			return false, err
		}
		if runs {
			proven[unit] = true
			provenOrder = append(provenOrder, unit)
		}
	}
	if len(provenOrder) == 0 {
		return false, nil
	}
	without := func(units []string) []string {
		kept := []string{}
		for _, unit := range units {
			if !proven[unit] {
				kept = append(kept, unit)
			}
		}
		return kept
	}
	restart.FailedOrStaleUnits = without(restart.FailedOrStaleUnits)
	restart.UnrestartedUnits = without(restart.UnrestartedUnits)
	for _, unit := range provenOrder {
		listed := false
		for _, service := range restart.RestartedServices {
			listed = listed || service == unit
		}
		if !listed {
			restart.RestartedServices = append(restart.RestartedServices, unit)
		}
	}
	for _, unit := range provenOrder {
		fmt.Fprintf(w, "  ✓ %s finished restarting onto the new code after the restart phase stopped waiting\n", unit)
	}
	restart.Incomplete = len(restart.FailedOrStaleUnits) > 0 || len(restart.PhaseErrors) > 0
	if !restart.Incomplete {
		fleetMarkHostRestarted(ctx, fleetRestartIdentitySHA(ctx))
	}
	return true, nil
}

// Names the pre update gateways and unmapped stops on the obligation once their restart is owed
func recordOwedGatewayInventory(ctx context.Context, plan *UpdatePlan, stoppedUnmappedPIDs []int) error {
	armed := fleetRestartObligationArmed(ctx)
	fields := map[string]string{}
	if armed {
		readable := false
		if fields, readable = fleetObligationFields(ctx); !readable {
			return nil
		}
	}
	recorded := []any{}
	if raw := fields["inventory"]; raw != "" && raw != "null" {
		inventory, err := jsonutil.Decode(raw)
		if err != nil {
			return nil
		}
		if _, _, err := markerOwedGateways(ctx, inventory); err != nil {
			var malformed *inventoryValueError
			if errors.As(err, &malformed) {
				return nil
			}
			return err
		}
		runtimes, isList := rowValue(inventory, "runtimes").([]any)
		if !isList {
			return fmt.Errorf("the recorded inventory holds no runtimes list")
		}
		recorded = runtimes
	}
	rows := []any{}
	if plan != nil {
		for _, runtime := range plan.Runtimes {
			if runtime != nil && runtime.Kind == "gateway" && strings.TrimSpace(runtime.Profile) != "" && runtime.Profile != "unknown" {
				rows = append(rows, runtime.ToDict())
			}
		}
	}
	stopped := append([]int{}, stoppedUnmappedPIDs...)
	sort.Ints(stopped)
	for _, pid := range stopped {
		rows = append(rows, jsonutil.ObjectOf("kind", "gateway", "profile", nil, "pid", int64(pid), "stopped_unmapped", true))
	}
	owed := []any{}
	for _, row := range rows {
		known := false
		for _, existing := range recorded {
			known = known || anyutil.Equal(row, existing)
		}
		if !known {
			owed = append(owed, row)
		}
	}
	if len(owed) == 0 {
		return nil
	}
	sha := currentCheckoutSHA(ctx)
	if armed {
		sha = fields["expected_sha"]
	}
	_, err := writeFleetRestartPendingMarker(ctx, sha, append(append([]any{}, recorded...), owed...), "")
	return err
}

// True when the armed obligation names gateways the live fleet still does not prove current
func namedGatewaysStillOwed(ctx context.Context) (bool, error) {
	if !fleetRestartObligationArmed(ctx) {
		return false, nil
	}
	fields, readable := fleetObligationFields(ctx)
	if !readable || fields["inventory"] == "" || fields["inventory"] == "null" {
		return false, nil
	}
	obsolete, err := markerOnlyRestartObsolete(ctx)
	return !obsolete, err
}

// Comma joined decimal text of a pid list
func joinPIDs(pids []int) string {
	parts := make([]string, len(pids))
	for i, pid := range pids {
		parts[i] = strconv.Itoa(pid)
	}
	return strings.Join(parts, ", ")
}

// Reason of the gateway_restart follow-up naming the gateways owed and the pids stopped without a successor
func owedRestartReason(stopped []int) string {
	reason := gatewayOwedReason
	if len(stopped) > 0 {
		reason += fmt.Sprintf(" (stopped PIDs %s)", joinPIDs(stopped))
	}
	return reason + gatewayRecovery
}

// Verifies the fleet after the restart phase, keeping the restart owed as a follow-up or clearing the obligation
func verifyFleetAfterUpdate(ctx context.Context, w io.Writer, restart *gatewayRestartOutcome, plan *UpdatePlan, windowsGatewayResume any, updateComplete bool) error {

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Tip: You can now select a provider and model:")
	fmt.Fprintln(w, "  icebath model              # Select provider and model")

	fleetSnapshot := []*jsonutil.Object{}
	unitsSettled := false
	if err := bestEffort(ctx, "Fleet version verification failed: %s", func() error {
		preRestart, killed := restart.fleetProbeSignals()
		rowsExpected := fleetProbeExpected(plan, preRestart, windowsGatewayResume, restart.RestartedServices, killed)
		var err error
		if fleetSnapshot, err = fleetCollectSnapshot(ctx, restart, rowsExpected); err != nil {
			fleetSnapshot = []*jsonutil.Object{}
			return err
		}
		if unitsSettled, err = settleUnitsProvenCurrent(ctx, w, restart, fleetSnapshot); err != nil {
			return err
		}
		if fleetPrintMatrix(w, fleetSnapshot) {
			restart.Incomplete = true
			fleetSignalStaleSurvivors(ctx, w, fleetRowsAsAny(fleetSnapshot), restart, fleetDrainBudget(ctx))
		} else if len(fleetSnapshot) == 0 && rowsExpected {
			fmt.Fprintln(w, "\n⚠ Fleet version check returned no rows even though gateway runtimes were expected — verification incomplete.")
			restart.Incomplete = true
		}
		return nil
	}); err != nil {
		return err
	}
	warnIncompleteGatewayFleetRestart(w, restart.UnrestartedUnits)

	if err := bestEffort(ctx, "Runtime-outcome reconciliation failed: %s", func() error {
		if plan == nil || len(plan.Runtimes) == 0 {
			return nil
		}
		evidence := runtimeOutcomeEvidence{
			RestartedServices: restart.RestartedServices, RelaunchedProfiles: restart.RelaunchedProfiles,
			ExternallySupervisedProfiles: restart.ExternallySupervisedProfiles, KilledPIDs: restart.KilledPIDs,
			FailedUnits:         restart.FailedOrStaleUnits,
			ExternalGatewayPIDs: map[int]bool{}, LiveGatewayPIDs: liveGatewayPidsFromFleet(fleetSnapshot),
		}
		for _, row := range fleetSnapshot {
			if pid, ok := intValue(rowValue(row, "pid")); ok && updaterecord.RowIsExternal(row) {
				evidence.ExternalGatewayPIDs[int(pid)] = true
			}
		}
		outcomes := fleetMatchOutcomes(plan, evidence)
		if fleetReportUnaccounted(w, outcomes) {
			restart.Incomplete = true
		}
		if active := updaterecord.CurrentReceipt(ctx); active != nil && active.Data != nil {
			active.Data.Set("runtime_outcomes", fleetRowsAsAny(outcomes))
		}
		return nil
	}); err != nil {
		return err
	}

	if !restart.Incomplete {
		if err := bestEffort(ctx, "Owed-gateway check failed: %s", func() error {
			owed, err := fleetNamedStillOwed(ctx)
			if err != nil {
				return err
			}
			if owed {
				fmt.Fprintln(w, "  ⚠ A gateway owed a restart by a previous update is still not serving this checkout.")
				restart.Incomplete = true
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if len(restart.StoppedUnmappedPIDs) > 0 {
		restart.Incomplete = true
	}
	if restart.Incomplete {
		stopped := sortedPIDs(restart.StoppedUnmappedPIDs)
		fleetRecordFollowup(ctx, "gateway_restart", owedRestartReason(stopped), updaterecord.FollowupRetryDefault)
		if err := bestEffort(ctx, "Fleet restart inventory not recorded: %s", func() error {
			return recordOwedGatewayInventory(ctx, plan, stopped)
		}); err != nil {
			return err
		}
	}
	if unitsSettled {
		restart.recordReceipt(ctx, "", nil)
	}
	if err := bestEffort(ctx, "Update receipt finalize failed: %s", func() error {
		if receiptPath := fleetFinalizeReceipt(ctx, "success", fleetRowsAsAny(fleetSnapshot), ""); receiptPath != "" {
			pkgLog().Info(fmt.Sprintf("Update receipt written: %s", receiptPath))
		}
		return nil
	}); err != nil {
		return err
	}

	if restart.Incomplete {
		return nil
	}
	fleetClearMarker(ctx)
	if !updateComplete {
		return nil
	}
	if err := fleetAutoMigrate(ctx, w); err != nil {
		if isKeyboardInterrupt(err) {
			return err
		}
		pkgLog().Warn(fmt.Sprintf("Multiplex auto-migration after update failed: %s", launchErrorText(err)))
		fmt.Fprintf(w, "  ⚠ Gateway multiplex migration did not finish: %s (run `icebath gateway migrate` later)\n", launchErrorText(err))
	}
	return nil
}

// Fleet rows as generic values for the receipt and the survivor pass
func fleetRowsAsAny(rows []*jsonutil.Object) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}
