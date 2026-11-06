package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Raised when systemctl --user cannot reach the user D-Bus session, the message is the remediation
type UserSystemdUnavailableError struct{ Msg string }

func (e *UserSystemdUnavailableError) Error() string { return e.Msg }

// Raised when a system scope gateway operation runs without root
type SystemScopeRequiresRootError struct{ Msg, Action string }

func (e *SystemScopeRequiresRootError) Error() string { return e.Msg }

// Process exit the CLI performs after printing its guidance
type SystemExitError struct{ Code int }

func (e *SystemExitError) Error() string { return "exit " + strconv.Itoa(e.Code) }

// Raised by a checked systemctl run that exits non zero
type CalledProcessError struct {
	Cmd            []string
	ReturnCode     int
	Stdout, Stderr string
}

func (e *CalledProcessError) Error() string {
	return fmt.Sprintf("Command '%s' returned non-zero exit status %d.", strings.Join(e.Cmd, " "), e.ReturnCode)
}

const (
	GatewayLoopAlive                = "alive"
	GatewayLoopWedged               = "wedged"
	GatewayLoopUnknown              = "unknown"
	DefaultLoopLivenessStaleAfterS  = 90.0
	systemdRestartWaitFloorS        = 60.0
	systemctlStartTimeout           = 30 * time.Second
	systemctlStopTimeout            = 90 * time.Second
	lifecycleExitPollInterval       = 500 * time.Millisecond
	lifecycleUserBusPollInterval    = 200 * time.Millisecond
	loginctlEnableLingerTimeout     = 30 * time.Second
	loginctlShowUserTimeout         = 10 * time.Second
	escalateDefaultTermGraceS       = 5.0
	escalateDefaultKillWaitS        = 5.0
	tickProbeDefaultTimeoutS        = 1.0
	tickProbeDefaultStrikes         = 3
	tickProbeDefaultGapS            = 0.2
	pendingRestartFailedResultState = "exit-code"
)

var (
	lifecycleOut                   io.Writer = os.Stdout
	lifecycleNow                             = time.Now
	lifecycleSleep                           = time.Sleep
	lifecycleGeteuid                         = os.Geteuid
	lifecyclePidExists                       = pidExistsLive
	lifecycleTerminatePid                    = TerminatePid
	lifecycleProcessStart                    = GetProcessStartTime
	lifecycleSendSIGUSR1                     = sendSIGUSR1
	lifecycleRunningPid                      = func() (int, bool) { pid, ok, err := GetRunningPid("", true); return pid, ok && err == nil }
	lifecycleRuntimeStatus                   = func() *pyjson.Object { return ReadRuntimeStatus("") }
	lifecycleWritePlannedStop                = WritePlannedStopMarker
	lifecycleGetuser                         = pypwd.Getuser
	lifecycleRefreshSystemdUnit              = refreshSystemdUnitIfNeeded
	lifecycleUserSystemdSocketUp             = userSystemdSocketReady
	lifecyclePathExists                      = pathExistsSafe
	lifecycleRestartExitWaitBudget           = GetRestartExitWaitBudget
	lifecycleWaitForPIDExit                  = waitForPIDExit
	lifecycleSelectScope                     = SelectSystemdScope
	lifecycleRequireInstalled                = requireServiceInstalled
	lifecyclePreflightUser                   = preflightUserSystemd
	lifecycleReadProps                       = ReadSystemdUnitProperties
	lifecycleRunSystemctl                    = RunSystemctl
	lifecycleServiceName                     = GetServiceName
	lifecycleMainPID                         = SystemdMainPID
	lifecycleGracefulRestart                 = gracefulRestartViaSIGUSR1
	lifecycleRefuseForeign                   = RefuseForeignSystemdUnit
	lifecycleProbeLiveness                   = ProbeGatewayLoopLiveness
	lifecycleEscalate                        = escalateWedgedGateway
	lifecycleSyncHome                        = SyncHermesHomeFromSystemdUnit
	lifecycleRecoverPending                  = recoverPendingSystemdRestart
	lifecycleWaitRestart                     = waitForSystemdServiceRestart
	lifecycleStartLimited                    = systemdServiceIsStartLimited
	lifecycleLingerStatus                    = GetSystemdLingerStatus
	lifecycleWaitUserBus                     = waitForUserDbusSocket
)

// Installed by the package that ports hermes_cli/update_cmd_drain_report.py, returns the per poll drain report callback
var DrainProgressReporterHook func(ctx context.Context, h string, budgetS float64) func()

func lifecyclePrintf(format string, a ...any) { fmt.Fprintf(lifecycleOut, format, a...) }

// Prints a red cross line the way hermes_cli/cli_output.py print_error does
func printLifecycleError(text string) {
	fmt.Fprintln(lifecycleOut, cliui.Color("✗ "+text, cliui.Red))
}

// Scope label with its first letter upper cased
func scopeLabelTitle(system bool) string {
	label := ServiceScopeLabel(system)
	return strings.ToUpper(label[:1]) + label[1:]
}

// Sudo prefix, scope flag and user flag for printed hints of a scope
func systemdCLIBits(system bool) (sudo, scopeFlag, userFlag string) {
	if system {
		return "sudo ", " --system", ""
	}
	return "", "", "--user "
}

// Runs systemctl and raises CalledProcessError on a non zero exit
func runSystemctlChecked(ctx context.Context, args []string, system bool, timeout time.Duration) (CommandResult, error) {
	res, err := lifecycleRunSystemctl(ctx, args, system, timeout)
	if err != nil {
		return res, err
	}
	if res.ReturnCode != 0 {
		return res, &CalledProcessError{Cmd: append(SystemctlCmd(system), args...), ReturnCode: res.ReturnCode, Stdout: res.Stdout, Stderr: res.Stderr}
	}
	return res, nil
}

// Error unless the effective user is root, naming the system scope action
func requireRootForSystemService(action string) error {
	if lifecycleGeteuid() != 0 {
		return &SystemScopeRequiresRootError{Msg: fmt.Sprintf("System gateway %s requires root. Re-run with sudo.", action), Action: action}
	}
	return nil
}

// Prints the install hint for a missing unit
func printServiceNotInstalled(system bool) {
	sudo, scopeFlag, _ := systemdCLIBits(system)
	lifecyclePrintf("✗ Gateway service is not installed\n")
	lifecyclePrintf("  Run: %sicebath gateway install%s\n", sudo, scopeFlag)
}

// Exits 1 with the install hint when the unit of the scope is missing
func requireServiceInstalled(ctx context.Context, action string, system bool) error {
	unit, err := GetSystemdUnitPath(ctx, system)
	if err != nil {
		return err
	}
	if !pathExistsSafe(unit) {
		printServiceNotInstalled(system)
		return &SystemExitError{Code: 1}
	}
	return nil
}

// Polls pred every interval until it holds or timeout elapses
func lifecyclePollUntil(pred func() bool, timeout, interval time.Duration) bool {
	deadline := lifecycleNow().Add(timeout)
	for {
		if pred() {
			return true
		}
		if !lifecycleNow().Before(deadline) {
			return false
		}
		lifecycleSleep(interval)
	}
}

// Waits for a user systemd control socket and adopts its environment
func waitForUserDbusSocket(timeout time.Duration) bool {
	if !lifecyclePollUntil(lifecycleUserSystemdSocketUp, timeout, lifecycleUserBusPollInterval) {
		return false
	}
	ensureUserSystemdEnv()
	return true
}

// Waits for the D-Bus socket of another account without adopting it
func waitForTargetUserBus(uid int, timeout time.Duration) bool {
	bus := fmt.Sprintf("/run/user/%d/bus", uid)
	return lifecyclePollUntil(func() bool { return lifecyclePathExists(bus) }, timeout, lifecycleUserBusPollInterval)
}

// Runs loginctl enable-linger for the user, failures of the launch propagate
func loginctlEnableLinger(ctx context.Context, username string) (CommandResult, error) {
	return RunServiceCommand(ctx, loginctlEnableLingerTimeout, "loginctl", "enable-linger", username)
}

// Stripped stderr, else stdout, else the exit code of a finished command
func completedProcessDetail(res CommandResult) string {
	text := res.Stderr
	if text == "" {
		text = res.Stdout
	}
	if text == "" {
		text = fmt.Sprintf("exit %d", res.ReturnCode)
	}
	return pystr.Strip(text)
}

// Linger state of the user or the caller, nil with the reason when it cannot be determined
func GetSystemdLingerStatus(ctx context.Context, username string) (*bool, string) {
	if hostIsTermux() {
		return nil, "not supported in Termux"
	}
	if !IsLinux() {
		return nil, "not supported on this platform"
	}
	if _, err := serviceLookPath("loginctl"); err != nil {
		return nil, "loginctl not found"
	}
	if username == "" {
		username = os.Getenv("USER")
		if username == "" {
			username = os.Getenv("LOGNAME")
		}
	}
	if username == "" {
		pw, err := pypwd.Getpwuid(serviceGetuid())
		if err != nil {
			return nil, "could not determine current user"
		}
		username = pw.Name
	}
	res, err := RunServiceCommand(ctx, loginctlShowUserTimeout, "loginctl", "show-user", username, "--property=Linger", "--value")
	if err != nil {
		return nil, pythonExceptionText(err)
	}
	if res.ReturnCode != 0 {
		detail := completedProcessDetail(res)
		if detail == "" {
			detail = "loginctl query failed"
		}
		return nil, detail
	}
	value := strings.ToLower(pystr.Strip(res.Stdout))
	yes, no := true, false
	switch value {
	case "yes", "true", "1":
		return &yes, ""
	case "no", "false", "0":
		return &no, ""
	}
	if value == "" {
		value = "<empty>"
	}
	return nil, "unexpected loginctl output: " + value
}

// Text Python shows for a subprocess launch failure of the service helpers
func pythonExceptionText(err error) string {
	if errors.Is(err, ErrCommandNotFound) {
		return "[Errno 2] " + err.Error()
	}
	return err.Error()
}

// Builds the remediation message of an unreachable user D-Bus session
func userSystemdUnavailable(reason, fixHint string) error {
	return &UserSystemdUnavailableError{Msg: reason + "\n" +
		"  systemctl --user cannot reach the user D-Bus session in this shell.\n" +
		"\n" +
		"  To fix:\n" +
		fixHint + "\n" +
		"\n" +
		"  Alternative: run the gateway in the foreground (stays up until\n" +
		"  you exit / close the terminal):\n" +
		"    icebath gateway run"}
}

// Ensures systemctl --user reaches user systemd, enabling linger when allowed, else UserSystemdUnavailableError
func preflightUserSystemd(ctx context.Context, autoEnableLinger bool) error {
	ensureUserSystemdEnv()
	if lifecycleUserSystemdSocketUp() {
		return nil
	}
	username, err := lifecycleGetuser()
	if err != nil {
		return err
	}
	linger, lingerDetail := lifecycleLingerStatus(ctx, "")
	sudoHint := "  sudo loginctl enable-linger " + username
	if linger != nil && *linger {
		if lifecycleWaitUserBus(3 * time.Second) {
			return nil
		}
		return userSystemdUnavailable("User systemd control sockets are missing even though linger is enabled.",
			fmt.Sprintf("  systemctl start user@%d.service\n  (may require sudo; try again after the command succeeds)", serviceGetuid()))
	}
	if _, lookErr := serviceLookPath("loginctl"); autoEnableLinger && lookErr == nil {
		res, err := loginctlEnableLinger(ctx, username)
		if err != nil {
			return userSystemdUnavailable(fmt.Sprintf("loginctl enable-linger failed (%s).", pythonExceptionText(err)), sudoHint)
		}
		if res.ReturnCode == 0 {
			if lifecycleWaitUserBus(5 * time.Second) {
				lifecyclePrintf("✓ Enabled linger for %s — user D-Bus now available\n", username)
				return nil
			}
			name, err := lifecycleServiceName(ctx)
			if err != nil {
				return err
			}
			return userSystemdUnavailable("Linger was enabled, but the user D-Bus socket did not appear.",
				"  Log out and log back in, then re-run the command.\n  Or reboot and run: systemctl --user start "+name)
		}
		return userSystemdUnavailable("loginctl enable-linger was denied: "+completedProcessDetail(res), sudoHint)
	}
	if lingerDetail == "" {
		lingerDetail = "linger disabled"
	}
	return userSystemdUnavailable(fmt.Sprintf("User D-Bus session is not available (%s).", lingerDetail), sudoHint)
}

// Resolves the effective scope then enforces root or user bus reachability and an installed unit
func systemdScopePreamble(ctx context.Context, action string, system, requireInstalled, preflightUser bool) (bool, error) {
	system, err := lifecycleSelectScope(ctx, system)
	if err != nil {
		return system, err
	}
	if system {
		if err := requireRootForSystemService(action); err != nil {
			return system, err
		}
	} else if preflightUser {
		if err := lifecyclePreflightUser(ctx, true); err != nil {
			return system, err
		}
	}
	if requireInstalled {
		if err := lifecycleRequireInstalled(ctx, action, system); err != nil {
			return system, err
		}
	}
	return system, nil
}

// Pinned home of the scope's unit when it belongs to another install, empty otherwise
func ForeignSystemdUnitHome(ctx context.Context, system bool) (string, error) {
	unitPath, err := GetSystemdUnitPath(ctx, system)
	if err != nil {
		return "", err
	}
	unitHome, err := hermesHomePinnedByUnit(unitPath)
	if err != nil || unitHome == "" {
		return "", err
	}
	expanded, err := pypath.PathExpanduser(unitHome)
	if err != nil {
		return "", err
	}
	unit, err := pypath.Resolve(expanded)
	if err != nil {
		return "", nil
	}
	mine, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return "", nil
	}
	if unit == mine {
		return "", nil
	}
	if def, err := pypath.Resolve(home.AccountDefault()); err == nil && mine == def {
		return "", nil
	}
	return unitHome, nil
}

// Exits 1 with guidance instead of operating another install's unit
func RefuseForeignSystemdUnit(ctx context.Context, action string, system bool) error {
	unitHome, err := ForeignSystemdUnitHome(ctx, system)
	if err != nil || unitHome == "" {
		return err
	}
	name, err := lifecycleServiceName(ctx)
	if err != nil {
		return err
	}
	printLifecycleError(fmt.Sprintf("Refusing to %s %s: that %s unit runs ICEBATH_HOME=%s, but this process has ICEBATH_HOME=%s.\n"+
		"It belongs to another Icebath install. Run this from that install's ICEBATH_HOME.",
		action, name, ServiceScopeLabel(system), unitHome, home.Dir(ctx)))
	return &SystemExitError{Code: 1}
}

// Writes the planned stop marker for pid or the recorded gateway pid, swallowing every failure
func markPlannedStop(pid int, havePid bool) {
	defer func() { _ = recover() }()
	if !havePid {
		pid, havePid = lifecycleRunningPidNoCleanup()
	}
	if havePid {
		lifecycleWritePlannedStop(pid)
	}
}

var lifecycleRunningPidNoCleanup = func() (int, bool) {
	pid, ok, err := GetRunningPid("", false)
	return pid, ok && err == nil
}

// Starts the systemd gateway service of the scope after refreshing its unit
func SystemdStart(ctx context.Context, system bool) error {
	system, err := systemdScopePreamble(ctx, "start", system, true, true)
	if err != nil {
		return err
	}
	if err := lifecycleRefuseForeign(ctx, "start", system); err != nil {
		return err
	}
	if _, err := lifecycleRefreshSystemdUnit(ctx, system); err != nil {
		return err
	}
	name, err := lifecycleServiceName(ctx)
	if err != nil {
		return err
	}
	if _, err := runSystemctlChecked(ctx, []string{"start", name}, system, systemctlStartTimeout); err != nil {
		return err
	}
	lifecyclePrintf("✓ %s service started\n", scopeLabelTitle(system))
	return nil
}

// Stops the systemd gateway service of the scope after marking the stop as planned
func SystemdStop(ctx context.Context, system bool) error {
	system, err := systemdScopePreamble(ctx, "stop", system, true, false)
	if err != nil {
		return err
	}
	if err := lifecycleRefuseForeign(ctx, "stop", system); err != nil {
		return err
	}
	if err := lifecycleSyncHome(ctx, system); err != nil {
		return err
	}
	markPlannedStop(0, false)
	name, err := lifecycleServiceName(ctx)
	if err != nil {
		return err
	}
	if _, err := runSystemctlChecked(ctx, []string{"stop", name}, system, systemctlStopTimeout); err != nil {
		if errors.Is(err, ErrCommandTimeout) {
			lifecyclePrintf("Gateway %s service is still stopping after 90s; check `icebath gateway status` or logs for final shutdown state.\n", ServiceScopeLabel(system))
			return nil
		}
		return err
	}
	lifecyclePrintf("✓ %s service stopped\n", scopeLabelTitle(system))
	return nil
}

// Restarts the systemd gateway service gracefully, escalating a wedged loop and recovering a pending restart
func SystemdRestart(ctx context.Context, system bool) error {
	system, err := systemdScopePreamble(ctx, "restart", system, true, true)
	if err != nil {
		return err
	}
	if err := lifecycleRefuseForeign(ctx, "restart", system); err != nil {
		return err
	}
	if _, err := lifecycleRefreshSystemdUnit(ctx, system); err != nil {
		return err
	}
	pid, havePid := lifecycleRunningPid()
	if !havePid {
		if pid, havePid, err = lifecycleMainPID(ctx, system); err != nil {
			return err
		}
	}
	if havePid && lifecycleProbeLiveness(ctx, pid, LoopLivenessOptions{}) == GatewayLoopWedged {
		lifecyclePrintf("⚠ Gateway PID %d event loop is unresponsive — skipping graceful drain and forcing a bounded stop...\n", pid)
		lifecycleEscalate(pid, escalateDefaultTermGraceS, escalateDefaultKillWaitS)
		name, err := lifecycleServiceName(ctx)
		if err != nil {
			return err
		}
		if _, err := lifecycleRunSystemctl(ctx, []string{"reset-failed", name}, system, systemctlStartTimeout); err != nil {
			return err
		}
		if _, err := lifecycleRunSystemctl(ctx, []string{"restart", name}, system, systemctlStopTimeout); err != nil {
			return err
		}
		_, err = lifecycleWaitRestart(ctx, system, pid, true, 0, nil)
		return err
	}
	if havePid {
		action, err := systemdGracefulRestartAction(ctx, system, pid)
		if err != nil || action == "" {
			return err
		}
		return systemdResetAndRun(ctx, action, system, pid, true)
	}
	recovered, err := lifecycleRecoverPending(ctx, system, 0, false)
	if err != nil || recovered {
		return err
	}
	return systemdResetAndRun(ctx, "restart", system, 0, false)
}

// Drains the live gateway through SIGUSR1 and returns the systemctl verb still needed, empty when systemd owns the relaunch
func systemdGracefulRestartAction(ctx context.Context, system bool, pid int) (string, error) {
	budget, err := lifecycleRestartExitWaitBudget(ctx)
	if err != nil {
		return "", err
	}
	lifecyclePrintf("⏳ %s service restarting gracefully (PID %d) — waiting up to %.0fs for in-flight turns + drain...\n", scopeLabelTitle(system), pid, budget)
	hook := DrainProgressReporterHook
	if hook == nil {
		panic("gateway: the hermes_cli/update_cmd_drain_report.py port must install DrainProgressReporterHook")
	}
	if !lifecycleGracefulRestart(pid, budget, hook(ctx, "", budget)) {
		lifecyclePrintf("⚠ Graceful restart did not complete within %ds; forcing a service restart...\n", int(budget))
		return "restart", nil
	}
	observed := false
	ok, err := lifecycleWaitRestart(ctx, system, pid, true, 0, &observed)
	if err != nil || ok {
		return "", err
	}
	if observed {
		return "", nil
	}
	limited, err := lifecycleStartLimited(ctx, system)
	if err != nil || limited {
		return "", err
	}
	props, err := lifecycleReadProps(ctx, system)
	if err != nil || len(props) == 0 {
		return "", err
	}
	replacement, haveReplacement := systemdMainPIDFromProps(props)
	switch {
	case props["ActiveState"] == "active" || props["ActiveState"] == "activating" || props["ActiveState"] == "reloading",
		props["SubState"] == "auto-restart",
		haveReplacement && replacement != pid:
		return "", nil
	}
	lifecyclePrintf("⚠ Systemd did not relaunch the gateway after its graceful exit; starting the inactive service...\n")
	return "start", nil
}

// Clears failed state, runs the systemctl verb and waits for the relaunch, printing the start limit hint on rejection
func systemdResetAndRun(ctx context.Context, action string, system bool, previousPid int, havePrevious bool) error {
	name, err := lifecycleServiceName(ctx)
	if err != nil {
		return err
	}
	if _, err := lifecycleRunSystemctl(ctx, []string{"reset-failed", name}, system, systemctlStartTimeout); err != nil {
		return err
	}
	if _, err := runSystemctlChecked(ctx, []string{action, name}, system, systemctlStopTimeout); err != nil {
		var cpe *CalledProcessError
		if errors.As(err, &cpe) {
			limited := systemdErrorIndicatesStartLimit(cpe)
			if !limited {
				if limited, err = lifecycleStartLimited(ctx, system); err != nil {
					return err
				}
			}
			if limited {
				return printSystemdStartLimitWait(ctx, system)
			}
			return cpe
		}
		if errors.Is(err, ErrCommandTimeout) {
			lifecyclePrintf("Gateway %s service is still restarting after 90s; check `icebath gateway status` or logs for final state.\n", ServiceScopeLabel(system))
			return nil
		}
		return err
	}
	_, err = lifecycleWaitRestart(ctx, system, previousPid, havePrevious, 0, nil)
	return err
}

// Pid recorded in a runtime status record, zero when absent, unparsable or not positive
func runtimeStatePID(state *pyjson.Object) int {
	if state == nil {
		return 0
	}
	v, _ := state.Get("pid")
	n, ok := pyIntOf(v)
	if !ok || n.Sign() <= 0 || !n.IsInt64() {
		return 0
	}
	return int(n.Int64())
}

// Python int(value or 0) of a decoded JSON value
func pyIntOf(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case nil:
		return big.NewInt(0), true
	case bool:
		if x {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	case int:
		return big.NewInt(int64(x)), true
	case int64:
		return big.NewInt(x), true
	case *big.Int:
		return x, true
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil, false
		}
		n, _ := big.NewFloat(math.Trunc(x)).Int(nil)
		return n, true
	case string:
		if x == "" {
			return big.NewInt(0), true
		}
		return pyint.Parse(x)
	}
	return nil, false
}

// Gateway runtime status record, nil when unreadable
func readGatewayRuntimeStatus() (state *pyjson.Object) {
	defer func() {
		if recover() != nil {
			state = nil
		}
	}()
	return lifecycleRuntimeStatus()
}

// String form of a runtime status field the way f-strings print it
func runtimeField(state *pyjson.Object, key string) (any, bool) {
	if state == nil {
		return nil, false
	}
	return state.Get(key)
}

// Waits for the service to come back active with a new pid and a running runtime, recording any replacement seen
func waitForSystemdServiceRestart(ctx context.Context, system bool, previousPid int, havePrevious bool, timeoutS float64, replacementObserved *bool) (bool, error) {
	name, err := lifecycleServiceName(ctx)
	if err != nil {
		return false, err
	}
	scope := scopeLabelTitle(system)
	if timeoutS == 0 {
		if timeoutS, err = systemdRestartWaitTimeout(ctx, system); err != nil {
			return false, err
		}
	}
	deadline := lifecycleNow().Add(time.Duration(timeoutS * float64(time.Second)))
	printedRuntimeWait := false
	for lifecycleNow().Before(deadline) {
		props, err := lifecycleReadProps(ctx, system)
		if err != nil {
			return false, err
		}
		activeState, subState := props["ActiveState"], props["SubState"]
		newPid, haveNew := lifecycleRunningPid()
		if !haveNew {
			newPid, haveNew = systemdMainPIDFromProps(props)
		}
		runtimeState := readGatewayRuntimeStatus()
		runtimePid := runtimeStatePID(runtimeState)
		if havePrevious && replacementObserved != nil && !*replacementObserved &&
			((haveNew && newPid > 0 && newPid != previousPid) || (runtimePid > 0 && runtimePid != previousPid)) {
			*replacementObserved = true
		}
		if activeState == "active" && haveNew && (!havePrevious || newPid != previousPid) {
			if runtimePid != newPid {
				runtimeState = readGatewayRuntimeStatus()
				if runtimeState != nil && runtimeStatePID(runtimeState) != newPid {
					runtimeState = nil
				}
			}
			gatewayState, _ := runtimeField(runtimeState, "gateway_state")
			switch gatewayState {
			case "running", "degraded":
				lifecyclePrintf("✓ %s service restarted (PID %d)\n", scope, newPid)
				if gatewayState == "degraded" {
					lifecyclePrintf("⚠ %s gateway is DEGRADED — see `icebath gateway status`\n", scope)
				}
				return true, nil
			case "startup_failed":
				reason := "startup failed"
				if r, _ := runtimeField(runtimeState, "exit_reason"); pyTruthy(r) {
					reason = pyStrValue(r)
				}
				lifecyclePrintf("⚠ %s service process restarted (PID %d), but gateway startup failed: %s\n", scope, newPid, reason)
				return false, nil
			}
			if !printedRuntimeWait {
				lifecyclePrintf("⏳ %s service process started (PID %d); waiting for gateway runtime...\n", scope, newPid)
				printedRuntimeWait = true
			}
		}
		if activeState == "activating" && subState == "auto-restart" {
			lifecycleSleep(time.Second)
			continue
		}
		if systemdUnitIsStartLimited(props) {
			return false, printSystemdStartLimitWait(ctx, system)
		}
		lifecycleSleep(2 * time.Second)
	}
	sudo, _, userFlag := systemdCLIBits(system)
	lifecyclePrintf("⚠ %s service did not become active within %ds.\n  Check status: %sicebath gateway status\n  Check logs:   journalctl %s-u %s -l --since '2 min ago'\n",
		scope, int(timeoutS), sudo, userFlag, name)
	return false, nil
}

// Python truthiness of a decoded JSON value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	case *big.Int:
		return x.Sign() != 0
	case []any:
		return len(x) > 0
	case *pyjson.Object:
		return x.Len() > 0
	}
	return true
}

// Python str of a decoded JSON scalar
func pyStrValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Runtime wait floor plus the unit's restart delay and start timeout
func systemdRestartWaitTimeout(ctx context.Context, system bool) (float64, error) {
	props, err := lifecycleReadProps(ctx, system, "RestartUSec", "TimeoutStartUSec")
	if err != nil {
		return 0, err
	}
	budget := 0.0
	for _, name := range []string{"RestartUSec", "TimeoutStartUSec"} {
		raw := props[name]
		var us *big.Int
		if pystr.StrIsDigit(raw) {
			us, _ = pyint.Parse(raw)
		} else {
			us = ParseSystemdDurationToUS(raw)
		}
		if us != nil {
			f, _ := new(big.Float).SetInt(us).Float64()
			budget += f / 1_000_000
		}
	}
	return systemdRestartWaitFloorS + budget, nil
}

// True when the unit's result or sub state reports start-limit-hit
func systemdUnitIsStartLimited(props map[string]string) bool {
	return strings.ToLower(props["Result"]) == "start-limit-hit" || strings.ToLower(props["SubState"]) == "start-limit-hit"
}

// True when a failed systemctl run's output names the start limit
func systemdErrorIndicatesStartLimit(e *CalledProcessError) bool {
	var parts []string
	for _, v := range []string{e.Stderr, e.Stdout, e.Stdout} {
		if v != "" {
			parts = append(parts, v)
		}
	}
	text := strings.ToLower(strings.Join(parts, "\n"))
	return strings.Contains(text, "start-limit-hit") || strings.Contains(text, "start request repeated too quickly") || strings.Contains(text, "start-limit")
}

// True when the unit of the scope is held by systemd's start limit
func systemdServiceIsStartLimited(ctx context.Context, system bool) (bool, error) {
	props, err := lifecycleReadProps(ctx, system)
	if err != nil {
		return false, err
	}
	return systemdUnitIsStartLimited(props), nil
}

// Prints how to wait out or clear systemd's start limit
func printSystemdStartLimitWait(ctx context.Context, system bool) error {
	name, err := lifecycleServiceName(ctx)
	if err != nil {
		return err
	}
	sudo, scopeFlag, userFlag := systemdCLIBits(system)
	lifecyclePrintf("⏳ %s service is temporarily rate-limited by systemd.\n", scopeLabelTitle(system))
	lifecyclePrintf("  systemd is refusing another immediate start after repeated exits.\n")
	lifecyclePrintf("  Wait for the start-limit window to expire, then run: %sicebath gateway restart%s\n", sudo, scopeFlag)
	lifecyclePrintf("  Or clear the failed state manually: systemctl %sreset-failed %s\n", userFlag, name)
	lifecyclePrintf("  Check logs: journalctl %s-u %s -l --since '5 min ago'\n", userFlag, name)
	return nil
}

// Recovers a planned restart stuck in auto-restart or failed state, true when it handled the restart
func recoverPendingSystemdRestart(ctx context.Context, system bool, previousPid int, havePrevious bool) (bool, error) {
	props, err := lifecycleReadProps(ctx, system)
	if err != nil || len(props) == 0 {
		return false, err
	}
	requested, _ := runtimeField(lifecycleRuntimeStatus(), "restart_requested")
	if !pyTruthy(requested) {
		return false, nil
	}
	activeState := props["ActiveState"]
	if activeState == "activating" && props["SubState"] == "auto-restart" {
		lifecyclePrintf("⏳ Service restart already pending — waiting for systemd relaunch...\n")
		return lifecycleWaitRestart(ctx, system, previousPid, havePrevious, 0, nil)
	}
	if activeState == "failed" && (props["ExecMainStatus"] == strconv.Itoa(GatewayServiceRestartExitCode) || props["Result"] == pendingRestartFailedResultState) {
		name, err := lifecycleServiceName(ctx)
		if err != nil {
			return false, err
		}
		lifecyclePrintf("↻ Clearing failed state for pending %s service restart...\n", ServiceScopeLabel(system))
		if _, err := lifecycleRunSystemctl(ctx, []string{"reset-failed", name}, system, systemctlStartTimeout); err != nil {
			return false, err
		}
		if _, err := lifecycleRunSystemctl(ctx, []string{"start", name}, system, systemctlStopTimeout); err != nil {
			return false, err
		}
		return lifecycleWaitRestart(ctx, system, previousPid, havePrevious, 0, nil)
	}
	return false, nil
}

// Agent section of the raw config of the home in effect, empty when it is not a mapping
func agentConfigSection(ctx context.Context) (map[string]any, bool, error) {
	cfg, err := config.ReadRawConfig(ctx, home.Dir(ctx))
	if err != nil {
		return nil, false, err
	}
	agent, ok := cfg["agent"].(map[string]any)
	return agent, ok, nil
}

// Configured restart drain timeout from the environment or agent.restart_drain_timeout
func getRestartDrainTimeout(ctx context.Context) (float64, error) {
	raw := pystr.Strip(os.Getenv("ICEBATH_RESTART_DRAIN_TIMEOUT"))
	if raw == "" {
		agent, _, err := agentConfigSection(ctx)
		if err != nil {
			return 0, err
		}
		v, ok := agent["restart_drain_timeout"]
		if !ok {
			v = DefaultGatewayRestartDrainTimeout
		}
		raw = pyStrConfig(v)
	}
	return ParseRestartDrainTimeout(raw), nil
}

// Python str of a raw config value
func pyStrConfig(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		return pyrFloat(x)
	}
	return fmt.Sprint(v)
}

// Python repr of a float
func pyrFloat(f float64) string {
	if f == math.Trunc(f) && !math.IsInf(f, 0) && math.Abs(f) < 1e16 {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Parsed environment override when set, else the parsed agent config key, else parse of None
func agentTimeoutSetting(ctx context.Context, envVar, key string, parse func(any) float64) (float64, error) {
	if raw, ok := os.LookupEnv(envVar); ok && pystr.Strip(raw) != "" {
		return parse(raw), nil
	}
	agent, isMap, err := agentConfigSection(ctx)
	if err != nil {
		return 0, err
	}
	if v, ok := agent[key]; isMap && ok {
		return parse(v), nil
	}
	return parse(nil), nil
}

// Configured cron and api server drain floor in seconds
func getCronDrainTimeout(ctx context.Context) (float64, error) {
	return agentTimeoutSetting(ctx, "ICEBATH_CRON_DRAIN_TIMEOUT", "cron_drain_timeout", ParseCronDrainTimeout)
}

// CLI wait for gateway exit after SIGUSR1 covering the after turn wait, drain and cron floor
func GetRestartExitWaitBudget(ctx context.Context) (float64, error) {
	drain, err := getRestartDrainTimeout(ctx)
	if err != nil {
		return 0, err
	}
	afterTurn, err := agentTimeoutSetting(ctx, "ICEBATH_RESTART_AFTER_TURN_TIMEOUT", "restart_after_turn_timeout", ParseRestartAfterTurnTimeout)
	if err != nil {
		return 0, err
	}
	cron, err := getCronDrainTimeout(ctx)
	if err != nil {
		return 0, err
	}
	return ResolveRestartExitWaitBudget(drain, afterTurn, cron, nil)
}

// Sends the drain aware restart signal and waits for the exit, false when unsent or still alive at the timeout
func gracefulRestartViaSIGUSR1(pid int, drainTimeout float64, onProgress func()) bool {
	if !hasSIGUSR1 || pid <= 0 {
		return false
	}
	switch err := lifecycleSendSIGUSR1(pid); {
	case errors.Is(err, errProcessGone):
		return true
	case err != nil:
		return false
	}
	return lifecycleWaitForPIDExit(pid, math.Max(drainTimeout, 1.0), onProgress)
}

// Waits up to timeout seconds for pid to exit, reporting progress on every poll
func waitForPIDExit(pid int, timeoutS float64, onProgress func()) bool {
	if pid <= 0 {
		return true
	}
	deadline := lifecycleNow().Add(time.Duration(math.Max(timeoutS, 0) * float64(time.Second)))
	for {
		if !lifecyclePidExists(pid) {
			return true
		}
		if !lifecycleNow().Before(deadline) {
			return false
		}
		if onProgress != nil {
			onProgress()
		}
		lifecycleSleep(lifecycleExitPollInterval)
	}
}

// Bounded SIGTERM then SIGKILL of a gateway whose loop is proven dead, true once it is gone
func escalateWedgedGateway(pid int, termGraceS, killWaitS float64) bool {
	expected := lifecycleProcessStart(pid)
	if err := lifecycleTerminatePid(pid, false, nil); err != nil {
		return lifecycleWaitForPIDExit(pid, 1.0, nil)
	}
	if lifecycleWaitForPIDExit(pid, math.Max(termGraceS, 0), nil) {
		return true
	}
	if err := lifecycleTerminatePid(pid, true, expected); err == nil {
		lifecyclePrintf("⚠ Gateway PID %d unresponsive to SIGTERM; sent SIGKILL\n", pid)
	}
	return lifecycleWaitForPIDExit(pid, math.Max(killWaitS, 0), nil)
}

// Microseconds of a systemd duration such as 1min 30s, nil on anything unexpected
func ParseSystemdDurationToUS(raw string) *big.Int {
	if raw == "" {
		return nil
	}
	units := map[string]int64{"us": 1, "ms": 1_000, "s": 1_000_000, "sec": 1_000_000,
		"min": 60_000_000, "h": 3_600_000_000, "hr": 3_600_000_000,
		"d": 86_400_000_000, "w": 604_800_000_000,
		"month": 2_629_800_000_000, "y": 31_557_600_000_000}
	total := new(big.Int)
	token, digits := "", ""
	flush := func() bool {
		multiplier := int64(1_000_000)
		if token != "" {
			m, ok := units[strings.ToLower(token)]
			if !ok {
				return false
			}
			multiplier = m
		}
		if digits == "" {
			return false
		}
		f, err := pyint.ParseFloat(digits)
		if err != nil {
			return false
		}
		product := f * float64(multiplier)
		if math.IsInf(product, 0) || math.IsNaN(product) {
			return false
		}
		n, _ := big.NewFloat(math.Trunc(product)).Int(nil)
		total.Add(total, n)
		digits, token = "", ""
		return true
	}
	for _, ch := range raw + " " {
		switch {
		case pystr.IsDigit(ch) || ch == '.':
			if token != "" && !flush() {
				return nil
			}
			digits += string(ch)
		case pystr.IsAlpha(ch):
			token += string(ch)
		default:
			if digits != "" && !flush() {
				return nil
			}
		}
	}
	if total.Sign() > 0 {
		return total
	}
	return nil
}
