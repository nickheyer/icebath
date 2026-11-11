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

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	serveOnlyReasonPrefix = "serve/dashboard "
	gatewayOwedReason     = "gateways may still run pre-update code or were stopped without a successor"
	gatewayRecovery       = "; recover with `icebath gateway restart`"
)

// Seconds the fleet probe keeps polling for a restarted gateway to publish its state
var fleetProbeSettleTimeoutSeconds = 120.0

// Calls of the verification pass the hermes tests replace
var (
	fleetPrintLegacyUnits     = printLegacyUnitsWarning
	fleetHasLegacyUnits       = gateway.HasLegacyHermesUnits
	fleetFindLegacyUnits      = gateway.FindLegacyHermesUnits
	fleetRefreshDashboard     = refreshDashboardAfterUpdate
	fleetCollectSnapshot      = collectFleetSnapshot
	fleetProbeExpected        = fleetProbeExpectedRuntimes
	fleetPrintMatrix          = printFleetVersionMatrixTo
	fleetSignalStaleSurvivors = signalStaleFleetSurvivors
	fleetMatchOutcomes        = matchRuntimeOutcomes
	fleetReportUnaccounted    = reportUnaccountedRuntimes
	fleetDeferManualServe     = DeferManualServe
	fleetRecordFollowup       = updaterecord.RecordFollowup
	fleetFinalizeReceipt      = updaterecord.FinalizeUpdateReceipt
	fleetAutoMigrate          = maybeAutoMigrateAfterUpdate
	fleetNamedStillOwed       = namedGatewaysStillOwed
)

// Prints the post update fleet matrix to w, true when a gateway is provably stale or down
func printFleetVersionMatrixTo(w io.Writer, fleet []*pyjson.Object) bool {
	saved := fleetStdout
	fleetStdout = w
	defer func() { fleetStdout = saved }()
	return PrintFleetVersionMatrix(fleet)
}

// Warns on every update about pre rename gateway units that fight the current unit for the bot token
func printLegacyUnitsWarning(ctx context.Context, w io.Writer) {
	if !(fleetSupportsSystemd(ctx) && fleetHasLegacyUnits(ctx)) {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "⚠ Legacy Icebath gateway unit(s) detected:")
	for _, unit := range fleetFindLegacyUnits(ctx) {
		scope := "user"
		if unit.System {
			scope = "system"
		}
		fmt.Fprintf(w, "    %s  (%s scope)\n", unit.Path, scope)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  These pre-rename units (icebath.service) fight the current")
	fmt.Fprintln(w, "  icebath-gateway.service for the bot token and cause SIGTERM")
	fmt.Fprintln(w, "  flap loops. Remove them with:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "    icebath gateway migrate-legacy")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  (add `sudo` if any are in system scope)")
}

// Fleet version rows, polled over a bounded settle window when runtimes are expected
func collectFleetSnapshot(ctx context.Context, restart *gatewayRestartOutcome, rowsExpected bool) ([]*pyjson.Object, error) {
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
		var unstamped []*pyjson.Object
		down := false
		for _, row := range snapshot {
			if fleetRowIdentityPending(row, prePIDs) {
				unstamped = append(unstamped, row)
			}
			down = down || pyrepr.Equal(rowValue(row, "state"), "down")
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
	if !pyrepr.Equal(rowValue(row, "state"), "unknown") || pyrepr.Truthy(rowValue(row, "code_sha")) {
		return false
	}
	if preRestartPIDs == nil {
		return false
	}
	pid, ok := pyIntValue(rowValue(row, "pid"))
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
	for _, line := range pystr.Splitlines(stdout, false) {
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
func liveGatewayPidsFromFleet(fleetRows []*pyjson.Object) map[string]map[int]bool {
	live := map[string]map[int]bool{}
	for _, row := range fleetRows {
		profile := rowValue(row, "profile")
		pid, isInt := pyIntValue(rowValue(row, "pid"))
		if !pyrepr.Truthy(profile) || !isInt || pyrepr.Equal(rowValue(row, "state"), "down") {
			continue
		}
		name := pyrepr.Str(profile)
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
			if isCommandMissing(err) || asCommandTimeout(err) != nil || pypath.IsOSError(err) {
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
			parsed, ok := pyint.Parse(pystr.Strip(text))
			if !ok || !parsed.IsInt64() {
				return false, nil
			}
			mainPID = int(parsed.Int64())
		}
		if !pids[mainPID] {
			return false, nil
		}
	}
	return loaded, nil
}

// Credits each failed gateway unit whose own main pid the fleet probe shows serving this checkout
func settleUnitsProvenCurrent(ctx context.Context, w io.Writer, restart *gatewayRestartOutcome, fleetRows []*pyjson.Object) (bool, error) {
	pre := restart.PreRestartGatewayPIDs
	if pre == nil || len(restart.FailedOrStaleUnits) == 0 {
		return false, nil
	}
	fresh := map[int]bool{}
	for _, row := range fleetRows {
		if pid, ok := pyIntValue(rowValue(row, "pid")); ok && pyrepr.Equal(rowValue(row, "state"), "current") {
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
		inventory, err := pyjson.Loads(raw)
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
			if runtime != nil && runtime.Kind == "gateway" && pystr.Strip(runtime.Profile) != "" && runtime.Profile != "unknown" {
				rows = append(rows, runtime.ToDict())
			}
		}
	}
	stopped := append([]int{}, stoppedUnmappedPIDs...)
	sort.Ints(stopped)
	for _, pid := range stopped {
		rows = append(rows, pyjson.ObjectOf("kind", "gateway", "profile", nil, "pid", int64(pid), "stopped_unmapped", true))
	}
	owed := []any{}
	for _, row := range rows {
		known := false
		for _, existing := range recorded {
			known = known || pyrepr.Equal(row, existing)
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

// True for a serve or dashboard unit name, bare or with its service suffix
func isServeUnit(name any) bool {
	return isServeOrDashboardUnit(strings.TrimSuffix(pyrepr.Str(name), ".service") + ".service")
}

// Whether the restart phase itself left a gateway owed, false when every recorded failure is a serve unit
func restartPhaseOwesGateway(restart *gatewayRestartOutcome) bool {
	if !restart.Incomplete {
		return false
	}
	if len(restart.PhaseErrors) > 0 || len(restart.FailedOrStaleUnits) == 0 {
		return true
	}
	for _, unit := range restart.FailedOrStaleUnits {
		if !isServeUnit(unit) {
			return true
		}
	}
	return false
}

// Whether a gateway is among the unaccounted outcomes, and the serve and dashboard pids among them
func splitUnaccounted(outcomes []*pyjson.Object) (bool, map[int]bool) {
	missedGateway, servePIDs := false, map[int]bool{}
	for _, outcome := range outcomes {
		if !pyrepr.Equal(rowValue(outcome, "outcome"), "unaccounted") {
			continue
		}
		kind, isStr := rowValue(outcome, "kind").(string)
		if !isStr || !serveKinds[kind] {
			missedGateway = true
			continue
		}
		if pid, ok := pyIntValue(rowValue(outcome, "pid")); ok {
			servePIDs[int(pid)] = true
		}
	}
	return missedGateway, servePIDs
}

// Comma joined decimal text of a pid list
func joinPIDs(pids []int) string {
	parts := make([]string, len(pids))
	for i, pid := range pids {
		parts[i] = strconv.Itoa(pid)
	}
	return strings.Join(parts, ", ")
}

// Reason of the gateway_restart follow-up naming what is owed, the gateway part first
func owedRestartReason(restart *gatewayRestartOutcome, stopped []int, gatewayOwed bool, staleServes, downServes map[int]bool) string {
	parts := []string{}
	if gatewayOwed {
		part := gatewayOwedReason
		if len(stopped) > 0 {
			part += fmt.Sprintf(" (stopped PIDs %s)", joinPIDs(stopped))
		}
		parts = append(parts, part+gatewayRecovery)
	}
	unitSet := map[string]bool{}
	for _, unit := range restart.FailedOrStaleUnits {
		if isServeUnit(unit) {
			unitSet[strings.TrimSuffix(unit, ".service")] = true
		}
	}
	if len(unitSet) > 0 {
		units := make([]string, 0, len(unitSet))
		for unit := range unitSet {
			units = append(units, unit)
		}
		sort.Strings(units)
		parts = append(parts, fmt.Sprintf("%sunit(s) %s were not restarted onto the new code; see the systemctl advice above", serveOnlyReasonPrefix, strings.Join(units, ", ")))
	}
	if len(downServes) > 0 {
		parts = append(parts, fmt.Sprintf("%sprocess(es) %s were stopped during the update and not brought back; relaunch `icebath serve` / `icebath dashboard`", serveOnlyReasonPrefix, joinPIDs(sortedPIDs(downServes))))
	}
	stillStale := map[int]bool{}
	for pid := range staleServes {
		if !downServes[pid] {
			stillStale[pid] = true
		}
	}
	if len(stillStale) > 0 {
		parts = append(parts, fmt.Sprintf("%sprocess(es) %s still run pre-update code; restart them as advised above", serveOnlyReasonPrefix, joinPIDs(sortedPIDs(stillStale))))
	}
	if len(parts) == 0 {
		return gatewayOwedReason + gatewayRecovery
	}
	return strings.Join(parts, "; ")
}

// Verifies the fleet after the restart phase, keeping the restart owed as a follow-up or clearing the obligation
func verifyFleetAfterUpdate(ctx context.Context, w io.Writer, restart *gatewayRestartOutcome, plan *UpdatePlan, windowsGatewayResume any, updateComplete bool) error {
	if err := bestEffort(ctx, "Legacy unit check during update failed: %s", func() error {
		fleetPrintLegacyUnits(ctx, w)
		return nil
	}); err != nil {
		return err
	}

	gatewayOwed := restartPhaseOwesGateway(restart)
	attempted := map[string]bool{}
	for _, unit := range restart.RestartedServices {
		attempted[unit] = true
	}
	for _, unit := range restart.FailedOrStaleUnits {
		attempted[unit] = true
	}
	dashboardsDown := fleetRefreshDashboard(ctx, w, attempted)
	if len(dashboardsDown) > 0 {
		restart.Incomplete = true
	}
	staleServesOwed := map[int]bool{}

	var staleServeRows []any
	staleServesKnown := false
	if err := bestEffort(ctx, "Failed to check for surviving serve runtimes: %s", func() error {
		rows, err := fleetSurvivingServeRuntimes(ctx, plan)
		if err != nil {
			return err
		}
		staleServeRows, staleServesKnown = rows, true
		if len(rows) > 0 {
			return fleetWarnStaleServes(w, rows)
		}
		return nil
	}); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Tip: You can now select a provider and model:")
	fmt.Fprintln(w, "  icebath model              # Select provider and model")

	fleetSnapshot := []*pyjson.Object{}
	unitsSettled := false
	if err := bestEffort(ctx, "Fleet version verification failed: %s", func() error {
		preRestart, killed := restart.fleetProbeSignals()
		rowsExpected := fleetProbeExpected(plan, preRestart, windowsGatewayResume, restart.RestartedServices, killed)
		var err error
		if fleetSnapshot, err = fleetCollectSnapshot(ctx, restart, rowsExpected); err != nil {
			fleetSnapshot = []*pyjson.Object{}
			return err
		}
		if unitsSettled, err = settleUnitsProvenCurrent(ctx, w, restart, fleetSnapshot); err != nil {
			return err
		}
		if unitsSettled {
			gatewayOwed = restartPhaseOwesGateway(restart)
			restart.Incomplete = restart.Incomplete || len(dashboardsDown) > 0
		}
		if fleetPrintMatrix(w, fleetSnapshot) {
			restart.Incomplete, gatewayOwed = true, true
			fleetSignalStaleSurvivors(ctx, w, fleetRowsAsAny(fleetSnapshot), restart, fleetDrainBudget(ctx))
		} else if len(fleetSnapshot) == 0 && rowsExpected {
			fmt.Fprintln(w, "\n⚠ Fleet version check returned no rows even though gateway runtimes were expected — verification incomplete.")
			restart.Incomplete, gatewayOwed = true, true
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
			FailedUnits: restart.FailedOrStaleUnits, FailedRespawnPIDs: dashboardsDown,
			ExternalGatewayPIDs: map[int]bool{}, LiveGatewayPIDs: liveGatewayPidsFromFleet(fleetSnapshot),
		}
		if staleServesKnown {
			evidence.StaleServePIDs = map[int]bool{}
			for _, row := range staleServeRows {
				if pid, ok := pyIntValue(rowValue(row, "pid")); ok {
					evidence.StaleServePIDs[int(pid)] = true
				}
			}
		}
		for _, row := range fleetSnapshot {
			if pid, ok := pyIntValue(rowValue(row, "pid")); ok && updaterecord.RowIsExternal(row) {
				evidence.ExternalGatewayPIDs[int(pid)] = true
			}
		}
		outcomes := fleetMatchOutcomes(plan, evidence)
		for i, runtime := range plan.Runtimes {
			if i >= len(outcomes) {
				break
			}
			if pyrepr.Equal(rowValue(outcomes[i], "outcome"), "unaccounted") && fleetDeferManualServe(ctx, runtime.ToDict(), true) {
				outcomes[i].Set("outcome", "deferred")
			}
		}
		if fleetReportUnaccounted(w, outcomes) {
			restart.Incomplete = true
			missedGateway, missedServes := splitUnaccounted(outcomes)
			gatewayOwed = gatewayOwed || missedGateway
			for pid := range missedServes {
				staleServesOwed[pid] = true
			}
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
				restart.Incomplete, gatewayOwed = true, true
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if len(restart.StoppedUnmappedPIDs) > 0 {
		restart.Incomplete, gatewayOwed = true, true
	}
	if restart.Incomplete {
		stopped := sortedPIDs(restart.StoppedUnmappedPIDs)
		fleetRecordFollowup(ctx, "gateway_restart", owedRestartReason(restart, stopped, gatewayOwed, staleServesOwed, dashboardsDown), updaterecord.FollowupRetryDefault)
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
			updateLog.Info(fmt.Sprintf("Update receipt written: %s", receiptPath))
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
		updateLog.Warn(fmt.Sprintf("Multiplex auto-migration after update failed: %s", launchErrorText(err)))
		fmt.Fprintf(w, "  ⚠ Gateway multiplex migration did not finish: %s (run `icebath gateway migrate` later)\n", launchErrorText(err))
	}
	return nil
}

// Fleet rows as generic values for the receipt and the survivor pass
func fleetRowsAsAny(rows []*pyjson.Object) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}
