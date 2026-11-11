package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	manualGatewaySkipReason = "manual gateway has no supervisor relaunch authority; left running for explicit operator restart"
	desktopServeSkipReason  = "desktop app owns and respawns this serve backend;" +
		" the recovery pass must not restart it out from under its supervisor"
	serveSkipReason = "no per-profile relaunch command reaches a serve/dashboard runtime; recovered by the fresh" +
		" systemd unit pass when a hermes-serve*/hermes-dashboard* unit owns it, else left running for explicit" +
		" operator restart"
	launchdServeSkipReason = "launchd job owns this backend; the post-update dashboard cleanup kickstarts the job through" +
		" launchd, never a detached argv respawn that would fight its KeepAlive"
)

// Gateway calls of the restart phase the hermes tests replace
var (
	fleetIsMacOS              = gateway.IsMacOS
	fleetSupportsSystemd      = gateway.SupportsSystemdServices
	fleetEnsureUserSystemdEnv = gateway.EnsureUserSystemdEnv
	fleetFindGatewayPIDs      = gateway.FindGatewayPIDsFor
	fleetGetServicePIDs       = gateway.GetServicePIDs
	fleetFindProfileProcesses = func(ctx context.Context, exclude map[int]bool) ([]gatewaycli.ProfileGatewayProcess, error) {
		return gatewaycli.FindProfileGatewayProcesses(ctx, exclude, false)
	}
	fleetPrepareProfileRestart = gateway.PrepareProfileGatewayUpdateRestart
	fleetWaitForGatewayExit    = gateway.WaitForGatewayExit
	fleetTerminatePid          = gateway.TerminatePid
	fleetProcessStartTime      = gateway.GetProcessStartTime
	fleetIsPidAncestor         = gateway.IsPidAncestorOfCurrentProcess
	fleetRequestSelfRestart    = gateway.RequestGatewaySelfRestart
	fleetProbeLoopLiveness     = func(ctx context.Context, pid int) string {
		return gateway.ProbeGatewayLoopLiveness(ctx, pid, gateway.LoopLivenessOptions{})
	}
	fleetEscalateWedged        = gateway.EscalateWedgedGateway
	fleetGracefulRestart       = gateway.GracefulRestartViaSIGUSR1
	fleetRestartExitWaitBudget = gateway.GetRestartExitWaitBudget
	fleetCollectVersionsWith   = CollectFleetVersions
	fleetHostRestartCompleted  = updaterecord.HostRestartAlreadyCompleted
	fleetMarkHostRestarted     = updaterecord.MarkHostRestartCompleted
	fleetRecordGatewayRestart  = updaterecord.RecordGatewayRestart

	fleetRestartSystemdUnits = restartSystemdGatewayUnits
	fleetRestartLaunchd      = restartMacosLaunchdGateways
	fleetRestartManual       = restartManualGateways
	fleetForceKillStuck      = forceKillStuckGateways
	fleetRecoverAfterAbort   = recoverAfterRestartPhaseAbort
	fleetDrainOrSignal       = drainOrSignalGatewayForUpdate
	fleetScopedManualPids    = scopedManualGatewayPids
	fleetDrainBudget         = gatewayDrainBudget
	fleetRestartIdentitySHA  = restartIdentitySHA
	fleetClearMarker         = clearFleetRestartPendingMarker

	fleetSurvivingServeRuntimes = survivingPreUpdateServeRuntimes
	fleetRecoverGatewayRestart  = recoverGatewayRestartAfterAbort
	fleetWarnStaleServes        = warnStaleServeRuntimes
	fleetOwedStaleServeRows     = owedStaleServeRows
	fleetAbortRecoveryComplete  = abortRecoveryIsComplete
)

// Restart phase bookkeeping, a nil PreRestartGatewayPIDs meaning the snapshot could not be read
type gatewayRestartOutcome struct {
	Incomplete                   bool
	PhaseErrors                  []string
	PreRestartGatewayPIDs        []int
	RestartedServices            []string
	FailedOrStaleUnits           []string
	RelaunchedProfiles           []string
	ExternallySupervisedProfiles []string
	KilledPIDs                   map[int]bool
	StoppedUnmappedPIDs          map[int]bool
	RestartedScopedUnits         map[string]bool
	SelfRestartPendingPIDs       map[int]bool
	UnrestartedUnits             []string
}

// Empty bookkeeping of a restart phase that has touched nothing yet
func newGatewayRestartOutcome() *gatewayRestartOutcome {
	return &gatewayRestartOutcome{
		PhaseErrors: []string{}, PreRestartGatewayPIDs: []int{}, RestartedServices: []string{}, FailedOrStaleUnits: []string{},
		RelaunchedProfiles: []string{}, ExternallySupervisedProfiles: []string{}, KilledPIDs: map[int]bool{},
		StoppedUnmappedPIDs: map[int]bool{}, RestartedScopedUnits: map[string]bool{}, SelfRestartPendingPIDs: map[int]bool{},
		UnrestartedUnits: []string{},
	}
}

// Pre restart pids and killed pids with the unmapped stops removed, the signals that predict a fleet row
func (o *gatewayRestartOutcome) fleetProbeSignals() ([]int, map[int]bool) {
	pre := o.PreRestartGatewayPIDs
	if pre != nil {
		pre = []int{}
		for _, pid := range o.PreRestartGatewayPIDs {
			if !o.StoppedUnmappedPIDs[pid] {
				pre = append(pre, pid)
			}
		}
	}
	killed := map[int]bool{}
	for pid := range o.KilledPIDs {
		if !o.StoppedUnmappedPIDs[pid] {
			killed[pid] = true
		}
	}
	return pre, killed
}

// Saves the current bookkeeping on the open receipt, never failing
func (o *gatewayRestartOutcome) recordReceipt(ctx context.Context, phaseError string, freshRecovery map[string]any) {
	fleetRecordGatewayRestart(ctx, updaterecord.GatewayRestart{
		RestartedServices: o.RestartedServices, RelaunchedProfiles: o.RelaunchedProfiles,
		ExternallySupervisedProfiles: o.ExternallySupervisedProfiles, KilledPIDs: sortedPIDs(o.KilledPIDs),
		FailedUnits: o.FailedOrStaleUnits, Incomplete: o.Incomplete, PhaseError: phaseError, FreshRecovery: freshRecovery,
	})
}

// Members of a pid set in ascending order
func sortedPIDs(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for pid := range set {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

// Fleet matrix when at least one gateway answers and every row is current at the checkout sha, else nil
func liveFleetCurrentRows(ctx context.Context) []any {
	checkoutSHA := currentCheckoutSHA(ctx)
	if checkoutSHA == "" {
		return nil
	}
	fleet, err := probeLiveFleet()
	if err != nil {
		fleetObligationLog.Debug(fmt.Sprintf("Pending fleet restart: fleet probe failed: %s", launchErrorText(err)))
		return nil
	}
	if len(fleet) == 0 {
		return nil
	}
	if _, identified := fleetCoveredGateways(fleet); !identified {
		return nil
	}
	for _, row := range fleet {
		if !pyrepr.Equal(rowValue(row, "state"), "current") || pyrepr.Str(rowValue(row, "code_sha")) != checkoutSHA {
			return nil
		}
	}
	return fleet
}

// Sha a completed host restart is stamped with, from the checkout, the obligation, then the latest receipt
func restartIdentitySHA(ctx context.Context) (sha string) {
	if sha = currentCheckoutSHA(ctx); sha != "" {
		return sha
	}
	if fields, ok := fleetObligationFields(ctx); ok {
		if sha = pystr.Strip(fields["expected_sha"]); sha != "" {
			return sha
		}
	}
	if postUpdate := rowValue(fleetLatestReceipt(ctx), "post_update"); isRowDict(postUpdate) {
		if value := rowValue(postUpdate, "sha"); pyrepr.Truthy(value) {
			return pyrepr.Str(value)
		}
	}
	return ""
}

// Why the completion tail may leave the fleet alone, false when a restart is owed
func fleetRestartSkipReason(ctx context.Context, plan *UpdatePlan) (string, bool) {
	checkoutSHA := fleetRestartIdentitySHA(ctx)
	if fleetHostRestartCompleted(checkoutSHA) {
		return "this host's gateway was already restarted for this update", true
	}
	if checkoutSHA == "" || plan == nil || len(plan.Runtimes) == 0 {
		return "", false
	}
	for _, runtime := range plan.Runtimes {
		if runtime.CodeSHA == nil || *runtime.CodeSHA != checkoutSHA {
			return "", false
		}
	}
	if liveFleetCurrentRows(ctx) == nil {
		return "", false
	}
	return "every running gateway already serves the checkout code", true
}

// Historical retry hook that stops this run so the ordinary completion owner retries
func runPendingFleetRestart(ctx context.Context) error {
	return pm.StopForRelaunchIncomplete(ctx)
}

// Prints the incomplete update warning naming every unit that was not restarted
func warnIncompleteGatewayFleetRestart(w io.Writer, failedUnits []string) {
	if len(failedUnits) == 0 {
		return
	}
	seen := map[string]bool{}
	ordered := []string{}
	for _, name := range failedUnits {
		if !seen[name] {
			seen[name] = true
			ordered = append(ordered, name)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "⚠ Update incomplete — some units were not restarted:")
	for _, name := range ordered {
		fmt.Fprintf(w, "    - %s\n", name)
	}
	if fleetIsMacOS() {
		fmt.Fprintln(w, "  Listed services may be deregistered from launchd, or still")
		fmt.Fprintln(w, "  running pre-update code (mixed sys.modules). Recover with:")
		fmt.Fprintln(w, "    icebath gateway status")
		fmt.Fprintln(w, "    launchctl list | grep <label>")
		fmt.Fprintln(w, "    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/<label>.plist")
		return
	}
	fmt.Fprintln(w, "  Skipped units may still be running pre-update code (mixed")
	fmt.Fprintln(w, "  sys.modules). Restart them manually, then verify:")
	fmt.Fprintln(w, "    icebath gateway status")
	units, labels := false, false
	for _, name := range ordered {
		if strings.HasPrefix(name, "ai.icebath.") {
			labels = true
		} else {
			units = true
		}
	}
	if units {
		fmt.Fprintln(w, "    systemctl --user restart <unit>   # user-scope")
		fmt.Fprintln(w, "    sudo systemctl restart <unit>     # system-scope")
	}
	if labels {
		fmt.Fprintln(w, "    launchctl kickstart -k gui/$UID/<label>   # macOS (or user/$UID)")
	}
}

// Pids of this update's gateways still running after the restart phase died, not known when the probe fails
func survivingGatewayPidsAfterFailedRestart(ctx context.Context) (pids []int, known bool) {
	found, err := fleetFindGatewayPIDs(ctx, nil, true)
	if err == nil {
		found, err = fleetScopedManualPids(ctx, io.Discard, found, nil, true)
	}
	if err != nil {
		updateLog.Debug(fmt.Sprintf("Could not probe for surviving gateways after update: %s", launchErrorText(err)))
		return nil, false
	}
	return found, true
}

// Fresh restart candidates of a pre update plan in plan order with each one's supervisor
type gatewayRecoveryCandidates struct {
	Profiles    []string
	Supervisors map[string]string
}

// Splits pre update runtimes into supervised gateways a fresh process may restart and skips with a reason
func gatewayRecoveryPartition(plan *UpdatePlan, skipProfiles map[string]bool) (gatewayRecoveryCandidates, []*pyjson.Object) {
	candidates := gatewayRecoveryCandidates{Profiles: []string{}, Supervisors: map[string]string{}}
	skipped := []*pyjson.Object{}
	if plan == nil {
		return candidates, skipped
	}
	for _, runtime := range plan.Runtimes {
		if runtime == nil || runtime.Profile == "" {
			continue
		}
		reason := ""
		switch runtime.Kind {
		case "gateway":
			if skipProfiles[runtime.Profile] {
				continue
			}
			if freshRestartSupervisors[runtime.Supervisor] {
				if _, seen := candidates.Supervisors[runtime.Profile]; !seen {
					candidates.Profiles = append(candidates.Profiles, runtime.Profile)
					candidates.Supervisors[runtime.Profile] = runtime.Supervisor
				}
				continue
			}
			reason = manualGatewaySkipReason
		case "serve", "dashboard":
			switch {
			case ClientOwnedServeSupervisors[runtime.Supervisor]:
				reason = desktopServeSkipReason
			case runtime.Supervisor == "launchd":
				reason = launchdServeSkipReason
			default:
				reason = serveSkipReason
			}
		default:
			continue
		}
		skipped = append(skipped, pyjson.ObjectOf("profile", runtime.Profile, "kind", runtime.Kind, "supervisor", runtime.Supervisor, "reason", reason))
	}
	return candidates, skipped
}

// Prints the recovery warning of a restart phase that raised, naming the pids still on pre update code
func warnGatewayRestartPhaseAborted(w io.Writer, phaseErr error, pids []int) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "⚠ Update incomplete — gateway auto-restart failed: %s\n", launchErrorText(phaseErr))
	if len(pids) > 0 {
		listed := make([]string, len(pids))
		for i, pid := range pids {
			listed[i] = strconv.Itoa(pid)
		}
		fmt.Fprintf(w, "  Gateway process(es) still running pre-update code: %s\n", strings.Join(listed, ", "))
	} else {
		fmt.Fprintln(w, "  Any gateway still running is serving pre-update code")
		fmt.Fprintln(w, "  (mixed sys.modules) against the updated checkout.")
	}
	fmt.Fprintln(w, "  Restart it manually, then verify:")
	fmt.Fprintln(w, "    icebath gateway restart")
	fmt.Fprintln(w, "    icebath gateway status")
}

// Hands a running gateway over to new code by ancestor self restart, wedged loop stop or graceful drain
func drainOrSignalGatewayForUpdate(ctx context.Context, w io.Writer, pid int, drainBudget float64, label string, selfRestartPending ...map[int]bool) (bool, error) {
	if fleetIsPidAncestor(pid) {
		fmt.Fprintf(w, "  → %s: update is running inside this gateway's process tree — signalling restart and letting the gateway drain itself (avoids the cron-update deadlock, #100179)\n", label)
		accepted := fleetRequestSelfRestart(pid)
		if accepted && len(selfRestartPending) > 0 && selfRestartPending[0] != nil {
			selfRestartPending[0][pid] = true
		}
		return accepted, nil
	}
	if fleetProbeLoopLiveness(ctx, pid) == gateway.GatewayLoopWedged {
		fmt.Fprintf(w, "  ⚠ %s: gateway event loop is unresponsive — skipping drain, forcing a bounded stop...\n", label)
		fleetEscalateWedged(pid)
		return true, nil
	}
	fmt.Fprintf(w, "  → %s: draining (up to %.0fs)...\n", label, drainBudget)
	progress := drainProgressReporter(ctx, gatewayHomeForPid(pid), drainBudget, drainReportIntervalS, func(report string) { fmt.Fprintln(w, report) })
	return fleetGracefulRestart(pid, drainBudget, progress), nil
}

// Home of the gateway a pid runs per the fleet inventory, empty when no profile state names it
func gatewayHomeForPid(pid int) (found string) {
	for _, profile := range fleetHomes() {
		record := gateway.ReadRuntimeStatus(filepath.Join(profile.home, "gateway_state.json"))
		if record == nil {
			continue
		}
		if recorded, ok := pyIntValue(rowValue(record, "pid")); ok && int(recorded) == pid {
			return profile.home
		}
	}
	return ""
}

// Sends SIGTERM to a pid, a vanished process or a refused signal reported as not sent
func fleetSigterm(pid int) (bool, error) {
	err := fleetTerminatePid(pid, false, nil)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, fs.ErrPermission) {
		return false, nil
	}
	return false, err
}

// Drains or stops every manual gateway of this update and prints the restart summary
func restartManualGateways(ctx context.Context, w io.Writer, out *gatewayRestartOutcome, drainBudget float64) error {
	listed, err := fleetGetServicePIDs(ctx, true)
	if err != nil {
		return err
	}
	servicePIDs := map[int]bool{}
	for _, pid := range listed {
		servicePIDs[pid] = true
	}
	for pid := range alreadyRestarted().PIDs {
		servicePIDs[pid] = true
	}
	manualPIDs, err := fleetFindGatewayPIDs(ctx, servicePIDs, true)
	if err != nil {
		return err
	}
	isManual := map[int]bool{}
	for _, pid := range manualPIDs {
		isManual[pid] = true
	}
	processes, err := fleetFindProfileProcesses(ctx, servicePIDs)
	if err != nil {
		return err
	}
	profileProcesses := map[int]gatewaycli.ProfileGatewayProcess{}
	profileOrder := []int{}
	keep := map[int]bool{}
	for _, proc := range processes {
		if !isManual[proc.PID] {
			continue
		}
		if _, seen := profileProcesses[proc.PID]; !seen {
			profileOrder = append(profileOrder, proc.PID)
		}
		profileProcesses[proc.PID] = proc
		keep[proc.PID] = true
	}
	if manualPIDs, err = fleetScopedManualPids(ctx, w, manualPIDs, keep, false); err != nil {
		return err
	}
	unrestartable := map[int]bool{}
	for _, pid := range profileOrder {
		proc := profileProcesses[pid]
		restartMode, armed, err := fleetPrepareProfileRestart(ctx, proc.Profile, pid)
		if err != nil {
			return err
		}
		if !armed {
			fmt.Fprintf(w, "  ⚠ %s: could not arm an automatic gateway restart for PID %d — stopping it instead so it cannot keep running pre-update code\n", proc.Profile, pid)
			unrestartable[pid] = true
			continue
		}
		signalled, err := fleetDrainOrSignal(ctx, w, pid, drainBudget, proc.Profile, out.SelfRestartPendingPIDs)
		if err != nil {
			return err
		}
		if !signalled {
			if _, err := fleetSigterm(pid); err != nil {
				return err
			}
		}
		fleetWaitForGatewayExit(5.0)
		out.KilledPIDs[pid] = true
		if restartMode == "external-supervisor" {
			out.ExternallySupervisedProfiles = append(out.ExternallySupervisedProfiles, proc.Profile)
		} else {
			out.RelaunchedProfiles = append(out.RelaunchedProfiles, proc.Profile)
		}
	}

	for _, pid := range manualPIDs {
		if _, mapped := profileProcesses[pid]; mapped && !unrestartable[pid] {
			continue
		}
		sent, err := fleetSigterm(pid)
		if err != nil {
			return err
		}
		if sent {
			out.KilledPIDs[pid] = true
			out.StoppedUnmappedPIDs[pid] = true
		}
	}

	if len(out.RestartedServices) > 0 || len(out.KilledPIDs) > 0 {
		fmt.Fprintln(w)
		for _, svc := range out.RestartedServices {
			fmt.Fprintf(w, "  ✓ Restarted %s\n", svc)
		}
		if len(out.RelaunchedProfiles) > 0 {
			fmt.Fprintf(w, "  ✓ Restarting manual gateway profile(s): %s\n", strings.Join(out.RelaunchedProfiles, ", "))
		}
		if len(out.ExternallySupervisedProfiles) > 0 {
			fmt.Fprintf(w, "  ✓ Handed gateway profile(s) back to their external supervisor: %s\n", strings.Join(out.ExternallySupervisedProfiles, ", "))
		}
		unmappedCount := len(out.KilledPIDs) - len(out.RelaunchedProfiles) - len(out.ExternallySupervisedProfiles)
		if unmappedCount != 0 {
			fmt.Fprintf(w, "  → Stopped %d manual gateway process(es)\n", unmappedCount)
			fmt.Fprintln(w, "    Restart manually: icebath gateway run")
			if unmappedCount > 1 {
				fmt.Fprintln(w, "    (or: icebath -p <profile> gateway run  for each profile)")
			}
		}
	}
	return nil
}

// Force kills the gateways this phase signalled that still run three seconds later
func forceKillStuckGateways(ctx context.Context, w io.Writer, killedPIDs map[int]bool) {
	_ = bestEffort(ctx, "Post-restart survivor sweep failed: %s", func() error {
		fleetSleep(3 * time.Second)
		listed, err := fleetGetServicePIDs(ctx, true)
		if err != nil {
			return err
		}
		servicePIDs := map[int]bool{}
		for _, pid := range listed {
			servicePIDs[pid] = true
		}
		surviving, err := fleetFindGatewayPIDs(ctx, servicePIDs, true)
		if err != nil {
			return err
		}
		stuck := []int{}
		for _, pid := range surviving {
			if killedPIDs[pid] {
				stuck = append(stuck, pid)
			}
		}
		if len(stuck) == 0 {
			return nil
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  ⚠ %d gateway process(es) ignored SIGTERM — force-killing\n", len(stuck))
		for _, pid := range stuck {
			_ = fleetTerminatePid(pid, true, fleetProcessStartTime(pid))
		}
		fleetSleep(1500 * time.Millisecond)
		return nil
	})
}

// Runs the fresh process recovery of an aborted restart phase and settles the fail closed verdict
func recoverAfterRestartPhaseAbort(ctx context.Context, w io.Writer, phaseErr error, plan *UpdatePlan, out *gatewayRestartOutcome, gatewayMode bool, restartedScopedUnits map[string]bool) error {
	updateLog.Debug(fmt.Sprintf("Gateway restart during update failed: %s", launchErrorText(phaseErr)))
	out.PhaseErrors = append(out.PhaseErrors, launchErrorText(phaseErr))
	surviving, survivorsKnown := survivingGatewayPidsAfterFailedRestart(ctx)
	planned := map[string]bool{}
	plannedOrder := []string{}
	if plan != nil {
		for _, runtime := range plan.Runtimes {
			if runtime != nil && runtime.Kind == "gateway" && !planned[runtime.Profile] {
				planned[runtime.Profile] = true
				plannedOrder = append(plannedOrder, runtime.Profile)
			}
		}
	}
	alreadyRestartedProfiles := map[string]bool{}
	for _, profile := range out.RelaunchedProfiles {
		alreadyRestartedProfiles[profile] = true
	}
	for _, profile := range out.ExternallySupervisedProfiles {
		alreadyRestartedProfiles[profile] = true
	}
	for _, profile := range plannedOrder {
		for _, service := range out.RestartedServices {
			if gatewayServiceMatchesProfile(profile, service) {
				alreadyRestartedProfiles[profile] = true
				break
			}
		}
	}
	skipUnits := map[string]bool{}
	for unit := range restartedScopedUnits {
		skipUnits[unit] = true
	}
	recoveryResult, err := fleetRecoverGatewayRestart(ctx, w, plan, gatewayMode, alreadyRestartedProfiles, skipUnits)
	if err != nil {
		return err
	}
	serveUnitsFailed := false
	if serveUnits := rowValue(recoveryResult["serve_units"], "failed"); pyrepr.Truthy(serveUnits) {
		serveUnitsFailed = true
	}
	staleRuntimeRows, err := fleetSurvivingServeRuntimes(ctx, plan)
	if err != nil {
		return err
	}
	recoveryResult["stale_runtimes"] = staleRuntimeRows
	verified := map[string]bool{}
	if rows, ok := recoveryResult["verified"].([]any); ok {
		for _, row := range rows {
			if profile, isStr := row.(string); isStr {
				verified[profile] = true
			}
		}
	} else if rows, ok := recoveryResult["verified"].([]string); ok {
		for _, profile := range rows {
			verified[profile] = true
		}
	}
	verifiedOrder := make([]string, 0, len(verified))
	for profile := range verified {
		verifiedOrder = append(verifiedOrder, profile)
	}
	sort.Strings(verifiedOrder)
	relaunched := map[string]bool{}
	for _, profile := range out.RelaunchedProfiles {
		relaunched[profile] = true
	}
	for _, profile := range verifiedOrder {
		if !relaunched[profile] {
			out.RelaunchedProfiles = append(out.RelaunchedProfiles, profile)
		}
	}
	covered := map[string]bool{}
	for profile := range alreadyRestartedProfiles {
		covered[profile] = true
	}
	for profile := range verified {
		covered[profile] = true
	}
	if fleetAbortRecoveryComplete(planned, covered, recoveryResult, staleRuntimeRows) {
		out.Incomplete = false
		if err := fleetWarnStaleServes(w, staleRuntimeRows); err != nil {
			return err
		}
	} else if restartPhaseFailureIsIncomplete(surviving, survivorsKnown, out.PreRestartGatewayPIDs) ||
		len(fleetOwedStaleServeRows(staleRuntimeRows)) > 0 || serveUnitsFailed {
		out.Incomplete = true
		var listed []int
		if survivorsKnown {
			listed = surviving
		}
		warnGatewayRestartPhaseAborted(w, phaseErr, listed)
		if err := fleetWarnStaleServes(w, staleRuntimeRows); err != nil {
			return err
		}
	}
	out.recordReceipt(ctx, launchErrorText(phaseErr), recoveryResult)
	return nil
}

// Seconds a drain first restart may wait for a gateway to exit, never under 45
func gatewayDrainBudget(ctx context.Context) (budget float64) {
	wait, err := fleetRestartExitWaitBudget(ctx)
	if err != nil {
		return 45.0
	}
	return math.Max(wait, 45.0)
}

// Restarts every running gateway of this update onto the pulled code and returns the phase bookkeeping
func restartGatewayFleetAfterUpdate(ctx context.Context, w io.Writer, plan *UpdatePlan, gatewayMode bool) (*gatewayRestartOutcome, error) {
	PublishStage("Restarting Hermes services")
	out := newGatewayRestartOutcome()
	restartedScopedUnits := map[string]bool{}

	phase := func() error {
		drainBudget := fleetDrainBudget(ctx)
		if found, findErr := fleetFindGatewayPIDs(ctx, nil, true); findErr != nil {
			out.PreRestartGatewayPIDs = nil
		} else if out.PreRestartGatewayPIDs, findErr = fleetScopedManualPids(ctx, io.Discard, found, nil, true); findErr != nil {
			out.PreRestartGatewayPIDs = nil
		}
		if err := fleetRestartSystemdUnits(ctx, w, &out.RestartedServices, &out.FailedOrStaleUnits, restartedScopedUnits, drainBudget, out.SelfRestartPendingPIDs); err != nil {
			return err
		}
		if fleetIsMacOS() {
			if err := fleetRestartLaunchd(ctx, w, &out.RestartedServices, &out.FailedOrStaleUnits, drainBudget, false, out.SelfRestartPendingPIDs); err != nil && !isCommandMissing(err) {
				return err
			}
		}
		if err := fleetRestartManual(ctx, w, out, drainBudget); err != nil {
			return err
		}
		if len(out.FailedOrStaleUnits) > 0 {
			out.Incomplete = true
		}
		out.UnrestartedUnits = append([]string{}, out.FailedOrStaleUnits...)
		out.recordReceipt(ctx, "", nil)
		fleetForceKillStuck(ctx, w, out.KilledPIDs)
		return nil
	}
	if err := phase(); err != nil {
		if isUpdateExit(err) || isKeyboardInterrupt(err) {
			return out, err
		}
		if recoverErr := fleetRecoverAfterAbort(ctx, w, err, plan, out, gatewayMode, restartedScopedUnits); recoverErr != nil {
			return out, recoverErr
		}
	}

	out.RestartedScopedUnits = map[string]bool{}
	for unit := range restartedScopedUnits {
		out.RestartedScopedUnits[unit] = true
	}
	if !out.Incomplete {
		fleetMarkHostRestarted(ctx, fleetRestartIdentitySHA(ctx))
	}
	return out, nil
}

// Whether an escaped restart phase failure must keep the restart owed, safe only when nothing ran before
func restartPhaseFailureIsIncomplete(surviving []int, survivorsKnown bool, preRestartPIDs []int) bool {
	if !survivorsKnown || len(surviving) > 0 {
		return true
	}
	return preRestartPIDs == nil || len(preRestartPIDs) > 0
}

// Whether the post update fleet probe should have produced rows, the Windows resume token never counted
func fleetProbeExpectedRuntimes(plan *UpdatePlan, preRestartPIDs []int, windowsResumeToken any, restartedServices []string, killedPIDs map[int]bool) bool {
	if len(restartedServices) > 0 || len(killedPIDs) > 0 {
		return true
	}
	if preRestartPIDs == nil || len(preRestartPIDs) > 0 {
		return true
	}
	if plan != nil {
		for _, runtime := range plan.Runtimes {
			if runtime != nil && runtime.Kind == "gateway" {
				return true
			}
		}
	}
	return false
}
