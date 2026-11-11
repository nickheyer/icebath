package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// One systemd manager scope and the systemctl prefix that addresses it
type systemdScope struct {
	Name string
	Cmd  []string
}

// One scope's list-units answer with the systemctl prefix that produced it
type systemdUnitListing struct {
	Scope    string
	ScopeCmd []string
	Listing  *pysubprocess.Completed
}

var (
	freshRestartSupervisors = map[string]bool{"systemd": true, "launchd": true, "service": true, "s6": true}
	systemdScopes           = []systemdScope{{"user", []string{"systemctl", "--user"}}, {"system", []string{"systemctl"}}}
	listGatewayUnits        = []string{"list-units", "icebath-gateway*", "icebath-serve*", "icebath-dashboard*", "--plain", "--no-legend", "--no-pager"}
	fatalExitParkRe         = pyre.MustCompile(fmt.Sprintf(`(?m)^RestartPreventExitStatus=.*\b%d\b`, gateway.GatewayFatalConfigExitCode))

	fleetRepairUnit        = repairUnitWithoutFatalExitPark
	fleetUnitOwned         = systemdUnitOwnedByUpdate
	fleetUnitMainPid       = unitMainPid
	fleetResetAndRestart   = systemctlResetAndRestart
	fleetSystemUnitDir     = gateway.SystemUnitDir
	fleetServiceName       = gateway.GetServiceName
	fleetRefreshUnit       = gateway.RefreshSystemdUnitIfNeeded
	fleetWaitServiceActive = waitForServiceActive

	fleetSleep     = time.Sleep
	fleetMonotonic = time.Now
	fleetEUID      = os.Geteuid

	// Runs one captured command under a timeout in seconds, the call the hermes tests replace
	fleetRunCommand = func(ctx context.Context, cmd []string, timeout float64) (*pysubprocess.Completed, error) {
		result, err := pysubprocess.RunWith(ctx, cmd, pysubprocess.Options{Timeout: time.Duration(timeout * float64(time.Second))})
		var expired *pysubprocess.TimeoutExpired
		if errors.As(err, &expired) && timeout != math.Trunc(timeout) {
			expired.TimeoutFloat = &timeout
		}
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	fleetSystemctl = func(ctx context.Context, cmd []string, timeout float64) (*pysubprocess.Completed, error) {
		return fleetRunCommand(ctx, cmd, timeout)
	}
)

// Copy of a command prefix with more arguments appended
func fleetArgv(prefix []string, more ...string) []string {
	out := make([]string, 0, len(prefix)+len(more))
	return append(append(out, prefix...), more...)
}

// Whether a failed command never started because its executable is missing
func isCommandMissing(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, gateway.ErrCommandNotFound)
}

// Timeout failure of a command, nil for any other error
func asCommandTimeout(err error) *pysubprocess.TimeoutExpired {
	var expired *pysubprocess.TimeoutExpired
	if errors.As(err, &expired) {
		return expired
	}
	if errors.Is(err, gateway.ErrCommandTimeout) {
		return &pysubprocess.TimeoutExpired{}
	}
	return nil
}

// Argv of a timed out command as Python prints the list, or the words unknown command
func timeoutCmdText(err error) string {
	expired := asCommandTimeout(err)
	if expired == nil || len(expired.Cmd) == 0 {
		return "unknown command"
	}
	items := make([]any, len(expired.Cmd))
	for i, arg := range expired.Cmd {
		items[i] = arg
	}
	return pyrepr.Repr(items)
}

// Captured stdout of a finished command as text with undecodable bytes replaced
func completedStdout(result *pysubprocess.Completed) string {
	if result == nil {
		return ""
	}
	return pystr.DecodeUTF8Replace(result.Stdout)
}

// Captured stderr of a finished command as text with undecodable bytes replaced
func completedStderr(result *pysubprocess.Completed) string {
	if result == nil {
		return ""
	}
	return pystr.DecodeUTF8Replace(result.Stderr)
}

// Runs one systemctl or sudo systemctl invocation capturing utf-8 text under a timeout in seconds
func systemctl(ctx context.Context, cmd []string, timeout float64) (*pysubprocess.Completed, error) {
	return fleetSystemctl(ctx, cmd, timeout)
}

// One listing per systemd scope that answered, a missing systemctl skipped and a timeout reported then skipped
func systemdGatewayUnitListings(ctx context.Context, onListTimeout func(scope string, err error)) ([]systemdUnitListing, error) {
	var listings []systemdUnitListing
	for _, scope := range systemdScopes {
		result, err := systemctl(ctx, fleetArgv(scope.Cmd, listGatewayUnits...), 10)
		if err != nil {
			if isCommandMissing(err) {
				continue
			}
			if asCommandTimeout(err) != nil {
				if onListTimeout != nil {
					onListTimeout(scope.Name, err)
				}
				continue
			}
			return listings, err
		}
		listings = append(listings, systemdUnitListing{Scope: scope.Name, ScopeCmd: scope.Cmd, Listing: result})
	}
	return listings, nil
}

// True for the system scope when this process is not root
func needsSudo(scope string) bool {
	return scope == "system" && fleetEUID() >= 0 && fleetEUID() != 0
}

// Live MainPID of a unit, zero when inactive, unprivileged or unreadable
func unitMainPid(ctx context.Context, scopeCmd []string, svcName string) (int, error) {
	result, err := systemctl(ctx, fleetArgv(scopeCmd, "show", svcName, "--property=MainPID", "--value"), 10)
	if err != nil {
		if pypath.IsOSError(err) || isCommandMissing(err) || asCommandTimeout(err) != nil {
			return 0, nil
		}
		return 0, err
	}
	if result == nil || result.Returncode != 0 {
		return 0, nil
	}
	text := pystr.Strip(completedStdout(result))
	if text == "" {
		return 0, nil
	}
	pid, ok := pyint.Parse(text)
	if !ok || !pid.IsInt64() {
		return 0, nil
	}
	return int(pid.Int64()), nil
}

// Scope qualified restart targets of every listing in discovery order
type systemdRestartTargets struct {
	keys  []string
	units map[string]systemdRestartTarget
}

// One unit to restart with the scope that listed it
type systemdRestartTarget struct {
	Scope    string
	ScopeCmd []string
	Unit     string
}

// Records a unit under its scope qualified key the first time it is seen
func (t *systemdRestartTargets) add(scope string, scopeCmd []string, unit string) {
	key := scope + "/" + unit
	if _, seen := t.units[key]; seen {
		return
	}
	if t.units == nil {
		t.units = map[string]systemdRestartTarget{}
	}
	t.units[key] = systemdRestartTarget{Scope: scope, ScopeCmd: scopeCmd, Unit: unit}
	t.keys = append(t.keys, key)
}

// Collapses units sharing one live main pid to one restart and names each covered legacy unit
func (t *systemdRestartTargets) collapse(ctx context.Context, w io.Writer) []string {
	keys := t.keys
	if len(keys) <= 1 {
		return keys
	}
	keys, covered := updaterecord.CollapseUnitsToHostProcesses(keys, func(key string) (int, error) {
		return fleetUnitMainPid(ctx, t.units[key].ScopeCmd, t.units[key].Unit)
	})
	for _, unitKey := range t.keys {
		ownerKey, isCovered := covered[unitKey]
		if !isCovered {
			continue
		}
		fmt.Fprintf(w, "  • %s is a legacy per-profile unit sharing one host gateway process with %s; restarting it again would restart that process twice. Fold the units together with: icebath gateway migrate\n", unitKey, ownerKey)
	}
	return keys
}

// Restarts every gateway and serve unit once per live host process, appending each failure to failed
func restartSystemdGatewayUnitsBestEffort(ctx context.Context, w io.Writer, failed *[]string, listings []systemdUnitListing) error {
	answered := map[string]bool{}
	targets := &systemdRestartTargets{}
	for _, listing := range listings {
		answered[listing.Scope] = true
		if listing.Listing.Returncode != 0 {
			*failed = append(*failed, fmt.Sprintf("systemd-%s (listing failed)", listing.Scope))
			continue
		}
		err := forEachSystemdGatewayUnit(completedStdout(listing.Listing),
			func(svcName string) error { targets.add(listing.Scope, listing.ScopeCmd, svcName); return nil },
			func(svcName string, _ error) { *failed = append(*failed, svcName) })
		if err != nil {
			return err
		}
	}
	for _, key := range targets.collapse(ctx, w) {
		target := targets.units[key]
		owned, err := fleetUnitOwned(ctx, w, target.ScopeCmd, target.Unit)
		if err != nil {
			return err
		}
		if !owned {
			continue
		}
		manageCmd := fleetArgv(target.ScopeCmd, "--no-ask-password")
		if needsSudo(target.Scope) {
			manageCmd = fleetArgv([]string{"sudo", "-n"}, manageCmd...)
		}
		result, err := fleetResetAndRestart(ctx, w, manageCmd, target.Unit, target.ScopeCmd)
		if err == nil && result.Returncode == 0 {
			var active bool
			if active, err = fleetWaitServiceActive(ctx, target.ScopeCmd, target.Unit, 10.0); err == nil && active {
				continue
			}
		}
		if err != nil && asCommandTimeout(err) == nil {
			return err
		}
		*failed = append(*failed, target.Unit)
	}
	for _, scope := range systemdScopes {
		if !answered[scope.Name] {
			*failed = append(*failed, fmt.Sprintf("systemd-%s (listing unavailable)", scope.Name))
		}
	}
	return nil
}

// Clears a unit's failed state then restarts it, waiting out the unit's own stop and start budgets
func systemctlResetAndRestart(ctx context.Context, w io.Writer, manageCmd []string, svcName string, scopeCmd []string) (*pysubprocess.Completed, error) {
	if scopeCmd == nil {
		scopeCmd = manageCmd
	}
	timeout, err := systemdRestartTimeout(ctx, scopeCmd, svcName, false)
	if err != nil {
		return nil, err
	}
	if _, err := systemctl(ctx, fleetArgv(manageCmd, "reset-failed", svcName), 10); err != nil {
		return nil, err
	}
	return runUnitJobTo(ctx, w, manageCmd, scopeCmd, "restart", svcName, timeout)
}

// True when the unit's home is one this update owns, a foreign or unreadable home announced and left alone
func systemdUnitOwnedByUpdate(ctx context.Context, w io.Writer, scopeCmd []string, svcName string) (bool, error) {
	unitHome, known := systemdUnitHermesHome(ctx, scopeCmd, svcName)
	if known && gateway.HomeInUpdateScope(ctx, unitHome, nil) {
		return true, nil
	}
	announceSkippedRuntime(ctx, w, "systemd unit", svcName, unitHome, known)
	return false, nil
}

// Pids whose live home this update owns plus keep, every other gateway process named and left running
func scopedManualGatewayPids(ctx context.Context, w io.Writer, pids []int, keep map[int]bool, quiet bool) ([]int, error) {
	candidates := make([]int, 0, len(pids))
	for _, pid := range pids {
		if !keep[pid] {
			candidates = append(candidates, pid)
		}
	}
	owned, foreign := fleetPartitionPidsByScope(ctx, candidates, nil)
	if !quiet {
		for _, row := range foreign {
			announceSkippedRuntime(ctx, w, "gateway process", fmt.Sprintf("PID %d", row.PID), row.Home, row.Home != "")
		}
	}
	ownedSet := map[int]bool{}
	for _, pid := range owned {
		ownedSet[pid] = true
	}
	out := []int{}
	for _, pid := range pids {
		if keep[pid] || ownedSet[pid] {
			out = append(out, pid)
		}
	}
	return out, nil
}

// True for a serve or dashboard service unit by the exact or hyphenated name shape
func isServeOrDashboardUnit(unit string) bool {
	return unit == "icebath-serve.service" || strings.HasPrefix(unit, "icebath-serve-") ||
		unit == "icebath-dashboard.service" || strings.HasPrefix(unit, "icebath-dashboard-")
}

// True for the base gateway unit, its hyphenated profile family and the serve and dashboard units
func isHermesGatewayUnit(unit string) bool {
	return unit == "icebath-gateway.service" || strings.HasPrefix(unit, "icebath-gateway-") || isServeOrDashboardUnit(unit)
}

// Runs processUnit per gateway and serve unit of a list-units output, a per unit timeout handed to onUnitTimeout
func forEachSystemdGatewayUnit(listUnitsStdout string, processUnit func(name string) error, onUnitTimeout func(name string, err error)) error {
	for _, line := range pystr.Splitlines(pystr.Strip(listUnitsStdout), false) {
		parts := pystr.SplitWhitespace(line, -1)
		if len(parts) == 0 {
			continue
		}
		unit := parts[0]
		if !strings.HasSuffix(unit, ".service") || !isHermesGatewayUnit(unit) {
			continue
		}
		svcName := strings.TrimSuffix(unit, ".service")
		if err := processUnit(svcName); err != nil {
			if asCommandTimeout(err) == nil {
				return err
			}
			onUnitTimeout(svcName, err)
		}
	}
	return nil
}

// True for the gateway units, the only ones whose process drains then restarts on SIGUSR1
func serviceUnitSupportsGracefulSigusr1Restart(svcName string) bool {
	return svcName == "icebath-gateway" || strings.HasPrefix(svcName, "icebath-gateway-")
}

// True when this user elevates without a prompt, by the blanket probe then the targeted one
func sudoNoninteractiveOk(ctx context.Context, targetedProbe []string) bool {
	blanket, err := fleetRunCommand(ctx, []string{"sudo", "-n", "true"}, 5)
	if err != nil {
		return false
	}
	if blanket.Returncode == 0 {
		return true
	}
	targeted, err := fleetRunCommand(ctx, fleetArgv([]string{"sudo", "-n"}, targetedProbe...), 5)
	return err == nil && targeted.Returncode == 0
}

// Command prefix for manage-units verbs in a scope, nil when no non interactive privilege path exists
func resolveManageCmd(ctx context.Context, cache map[string][]string, scope string, scopeCmd []string, svcName string) []string {
	if cached, seen := cache[scope]; seen {
		return cached
	}
	cmd := fleetArgv(scopeCmd, "--no-ask-password")
	if needsSudo(scope) {
		if sudoNoninteractiveOk(ctx, fleetArgv(cmd, "reset-failed", svcName)) {
			cmd = fleetArgv([]string{"sudo", "-n"}, cmd...)
		} else {
			cmd = nil
		}
	}
	cache[scope] = cmd
	return cmd
}

// Rewrites a root owned system unit that restarts on the fatal exit, else names the repair command
func repairUnitWithoutFatalExitPark(ctx context.Context, w io.Writer, svcName, scope string) error {
	system := scope == "system"
	unitDir := gateway.UserSystemdUnitDir(ctx)
	if system {
		unitDir = fleetSystemUnitDir()
	}
	data, err := os.ReadFile(pypath.PathJoin(unitDir, svcName+".service"))
	if err != nil {
		return nil
	}
	data = []byte(strings.TrimPrefix(string(data), string([]byte{0xef, 0xbb, 0xbf})))
	if !utf8.Valid(data) {
		return fmt.Errorf("'utf-8' codec can't decode the unit file of %s", svcName)
	}
	if parked, err := fatalExitParkRe.MatchString(string(data)); err != nil || parked {
		return err
	}
	if system && !needsSudo(scope) {
		current, err := fleetServiceName(ctx)
		if err != nil {
			return err
		}
		if svcName == current {
			launchHome, hadHome := os.LookupEnv("ICEBATH_HOME")
			_, err := fleetRefreshUnit(ctx, true)
			if hadHome {
				os.Setenv("ICEBATH_HOME", launchHome)
			} else {
				os.Unsetenv("ICEBATH_HOME")
			}
			return err
		}
	}
	code := gateway.GatewayFatalConfigExitCode
	sudo, flag := "", ""
	if system {
		sudo, flag = "sudo ", " --system"
	}
	fmt.Fprintf(w, "  ⚠ %s lacks RestartPreventExitStatus=%d: a permanent refusal (exit %d) would crash-loop it instead of parking.\n    Repair: %sicebath gateway install%s\n", svcName, code, code, sudo, flag)
	return nil
}

// Restarts one active unit by a graceful drain then a forced restart, recording where it settled
func restartOneSystemdGatewayUnit(ctx context.Context, w io.Writer, svcName, scope string, scopeCmd []string, drainBudget float64,
	manageCmdCache map[string][]string, restartedServices, failedOrStaleUnits *[]string, selfRestartPending map[int]bool) error {
	check, err := systemctl(ctx, fleetArgv(scopeCmd, "is-active", svcName), 5)
	if err != nil {
		return err
	}
	if pystr.Strip(completedStdout(check)) != "active" {
		return nil
	}
	if owned, err := fleetUnitOwned(ctx, w, scopeCmd, svcName); err != nil || !owned {
		return err
	}
	if err := fleetRepairUnit(ctx, w, svcName, scope); err != nil {
		return err
	}
	manageCmd := resolveManageCmd(ctx, manageCmdCache, scope, scopeCmd, svcName)

	mainPID := 0
	if serviceUnitSupportsGracefulSigusr1Restart(svcName) {
		show, err := systemctl(ctx, fleetArgv(scopeCmd, "show", svcName, "--property=MainPID", "--value"), 5)
		if err != nil {
			if asCommandTimeout(err) == nil && !isCommandMissing(err) {
				return err
			}
		} else if text := pystr.Strip(completedStdout(show)); text != "" {
			if pid, ok := pyint.Parse(text); ok && pid.IsInt64() {
				mainPID = int(pid.Int64())
			}
		}
	}

	gracefulOK := false
	if mainPID > 0 {
		if gracefulOK, err = fleetDrainOrSignal(ctx, w, mainPID, drainBudget, svcName, selfRestartPending); err != nil {
			return err
		}
	}
	if gracefulOK {
		if manageCmd != nil {
			if _, err := systemctl(ctx, fleetArgv(manageCmd, "reset-failed", svcName), 10); err != nil {
				return err
			}
			timeout, err := systemdRestartTimeout(ctx, scopeCmd, svcName, true)
			if err != nil {
				return err
			}
			if _, err := runUnitJobTo(ctx, w, manageCmd, scopeCmd, "start", svcName, timeout); err != nil {
				return err
			}
			active, err := fleetWaitServiceActive(ctx, scopeCmd, svcName, 10.0)
			if err != nil {
				return err
			}
			if active {
				*restartedServices = append(*restartedServices, svcName)
				return nil
			}
		}
		restartSec, err := serviceRestartSec(ctx, scopeCmd, svcName, 0.0)
		if err != nil {
			return err
		}
		if manageCmd == nil && restartSec > 5.0 {
			fmt.Fprintf(w, "  → %s: waiting for systemd auto-restart (~%ds; no root for an immediate restart)...\n", svcName, int(restartSec))
		}
		active, err := fleetWaitServiceActive(ctx, scopeCmd, svcName, math.Max(10.0, restartSec+10.0))
		if err != nil {
			return err
		}
		if active {
			*restartedServices = append(*restartedServices, svcName)
			return nil
		}
		fmt.Fprintf(w, "  ⚠ %s drained but didn't relaunch — forcing restart\n", svcName)
	}

	if manageCmd == nil {
		*failedOrStaleUnits = append(*failedOrStaleUnits, svcName)
		fmt.Fprintf(w, "  ⚠ %s is a system service and restarting it needs root.\n    Restart it manually to load the new version:\n      sudo systemctl restart %s\n    To let `icebath update` restart it automatically, allow\n    passwordless sudo for systemctl, or run updates with sudo.\n", svcName, svcName)
		return nil
	}

	restart, err := systemctlResetAndRestart(ctx, w, manageCmd, svcName, scopeCmd)
	if err != nil {
		return err
	}
	if restart.Returncode != 0 {
		*failedOrStaleUnits = append(*failedOrStaleUnits, svcName)
		fmt.Fprintf(w, "  ⚠ Failed to restart %s: %s\n", svcName, pystr.Strip(completedStderr(restart)))
		return nil
	}
	active, err := fleetWaitServiceActive(ctx, scopeCmd, svcName, 10.0)
	if err != nil {
		return err
	}
	if active {
		*restartedServices = append(*restartedServices, svcName)
		return nil
	}
	fmt.Fprintf(w, "  ⚠ %s died after restart, retrying...\n", svcName)
	if _, err := systemctlResetAndRestart(ctx, w, manageCmd, svcName, scopeCmd); err != nil {
		return err
	}
	if active, err = fleetWaitServiceActive(ctx, scopeCmd, svcName, 10.0); err != nil {
		return err
	}
	if active {
		*restartedServices = append(*restartedServices, svcName)
		fmt.Fprintf(w, "  ✓ %s recovered on retry\n", svcName)
		return nil
	}
	*failedOrStaleUnits = append(*failedOrStaleUnits, svcName)
	scopeFlag, sudoHint := "", ""
	if scope == "user" {
		scopeFlag = "--user "
	}
	if scope == "system" {
		sudoHint = "sudo "
	}
	fmt.Fprintf(w, "  ✗ %s failed to stay running after restart.\n    Check logs: %sjournalctl %s-u %s --since '2 min ago'\n    Recover manually:\n      %ssystemctl %sreset-failed %s\n      %ssystemctl %srestart %s\n",
		svcName, sudoHint, scopeFlag, svcName, sudoHint, scopeFlag, svcName, sudoHint, scopeFlag, svcName)
	return nil
}

// Restarts every active gateway and serve unit of both scopes, a timeout isolated to its unit
func restartSystemdGatewayUnits(ctx context.Context, w io.Writer, restartedServices, failedOrStaleUnits *[]string,
	restartedScopedUnits map[string]bool, drainBudget float64, selfRestartPending map[int]bool) error {
	if !fleetSupportsSystemd(ctx) {
		return nil
	}
	manageCmdCache := map[string][]string{}
	fleetEnsureUserSystemdEnv()

	onListTimeout := func(scope string, err error) {
		fmt.Fprintf(w, "  ⚠ systemctl timed out listing %s-scope gateway units (%s). Check the gateway with: icebath gateway status\n", scope, timeoutCmdText(err))
	}
	onUnitTimeout := func(svcName string, err error) {
		*failedOrStaleUnits = append(*failedOrStaleUnits, svcName)
		fmt.Fprintf(w, "  ⚠ systemctl timed out restarting %s (%s); continuing with remaining gateways\n", svcName, timeoutCmdText(err))
	}

	targets := &systemdRestartTargets{}
	listings, err := systemdGatewayUnitListings(ctx, onListTimeout)
	if err != nil {
		return err
	}
	for _, listing := range listings {
		err := forEachSystemdGatewayUnit(completedStdout(listing.Listing),
			func(svcName string) error { targets.add(listing.Scope, listing.ScopeCmd, svcName); return nil }, onUnitTimeout)
		if err != nil {
			return err
		}
	}
	for _, key := range targets.collapse(ctx, w) {
		target := targets.units[key]
		if alreadyRestarted().Units[key] {
			restartedScopedUnits[key] = true
			continue
		}
		scopeMark := len(*restartedServices)
		err := restartOneSystemdGatewayUnit(ctx, w, target.Unit, target.Scope, target.ScopeCmd, drainBudget, manageCmdCache,
			restartedServices, failedOrStaleUnits, selfRestartPending)
		for _, name := range (*restartedServices)[scopeMark:] {
			restartedScopedUnits[target.Scope+"/"+name] = true
		}
		if err != nil {
			if asCommandTimeout(err) == nil {
				return err
			}
			onUnitTimeout(target.Unit, err)
		}
	}
	return nil
}

// Polls is-active every half second until the unit is active or the timeout passes
func waitForServiceActive(ctx context.Context, scopeCmd []string, svcName string, timeout float64) (bool, error) {
	deadline := fleetMonotonic().Add(time.Duration(math.Max(timeout, 0.5) * float64(time.Second)))
	for {
		verify, err := systemctl(ctx, fleetArgv(scopeCmd, "is-active", svcName), 5)
		if err != nil && !isCommandMissing(err) && asCommandTimeout(err) == nil {
			return false, err
		}
		if err == nil && pystr.Strip(completedStdout(verify)) == "active" {
			return true, nil
		}
		if !fleetMonotonic().Before(deadline) {
			return false, nil
		}
		fleetSleep(500 * time.Millisecond)
	}
}
