package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Runs argv with its output streamed to w under a timeout, the uncaptured subprocess run
func runServiceInherited(ctx context.Context, w io.Writer, timeout time.Duration, argv ...string) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, w, os.Stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %q after %v", ErrCommandTimeout, argv, timeout)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	if err != nil && (errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)) {
		return fmt.Errorf("%w: %q", ErrCommandNotFound, argv[0])
	}
	return err
}

// Prints the systemd unit state, restart and linger notes, health and on deep the recent journal
func SystemdStatus(ctx context.Context, w io.Writer, deep, system, full bool, printRuntimeHealth func() error) error {
	system, err := SelectSystemdScope(ctx, system)
	if err != nil {
		return err
	}
	unitPath, err := GetSystemdUnitPath(ctx, system)
	if err != nil {
		return err
	}
	svc, err := GetServiceName(ctx)
	if err != nil {
		return err
	}
	scopeLabel := pystr.Capitalize(ServiceScopeLabel(system))
	sudo, scopeFlag, userFlag := systemdCLIBits(system)
	if !pathExistsSafe(unitPath) {
		printServiceNotInstalled(system)
		return nil
	}
	conflicting, err := hasConflictingSystemdUnits(ctx)
	if err != nil {
		return err
	}
	if conflicting {
		if err := printSystemdScopeConflictWarning(ctx, w); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	if hasLegacyHermesUnits(ctx) {
		printLegacyUnitWarning(ctx, w)
		fmt.Fprintln(w)
	}
	current, err := SystemdUnitIsCurrent(ctx, system)
	if err != nil {
		return err
	}
	if !current {
		fmt.Fprintln(w, "⚠ Installed gateway service definition is outdated")
		fmt.Fprintf(w, "  Run: %sicebath gateway restart%s  # auto-refreshes the unit\n", sudo, scopeFlag)
		fmt.Fprintln(w)
	}
	statusCmd := []string{"status", svc, "--no-pager"}
	if full {
		statusCmd = append(statusCmd, "-l")
	}
	if err := runServiceInherited(ctx, w, 10*time.Second, append(SystemctlCmd(system), statusCmd...)...); err != nil {
		if errors.Is(err, ErrCommandNotFound) {
			return SystemctlUnavailableError{}
		}
		return err
	}
	result, err := RunSystemctl(ctx, []string{"is-active", svc}, system, 10*time.Second)
	if err != nil {
		return err
	}
	if pystr.Strip(result.Stdout) == "active" {
		fmt.Fprintf(w, "✓ %s gateway service is running\n", scopeLabel)
	} else {
		fmt.Fprintf(w, "✗ %s gateway service is stopped\n", scopeLabel)
		fmt.Fprintf(w, "  Run: %sicebath gateway start%s\n", sudo, scopeFlag)
	}
	if system {
		configuredUser, err := readSystemdUserFromUnit(unitPath)
		if err != nil {
			return err
		}
		if configuredUser != "" {
			fmt.Fprintf(w, "Configured to run as: %s\n", configuredUser)
		}
	}
	if err := printRuntimeHealth(); err != nil {
		return err
	}
	props, err := ReadSystemdUnitProperties(ctx, system)
	if err != nil {
		return err
	}
	activeState, resultCode := props["ActiveState"], props["Result"]
	switch {
	case activeState == "activating" && props["SubState"] == "auto-restart":
		fmt.Fprintln(w, "  ⏳ Restart pending: systemd is waiting to relaunch the gateway")
	case systemdUnitIsStartLimited(props):
		fmt.Fprintln(w, "  ⏳ Restart pending: systemd is temporarily rate-limiting starts")
		fmt.Fprintf(w, "  Run after the start-limit window expires: %sicebath gateway restart%s\n", sudo, scopeFlag)
		fmt.Fprintf(w, "  Or clear it manually: systemctl %sreset-failed %s\n", userFlag, svc)
	case activeState == "failed" && props["ExecMainStatus"] == strconv.Itoa(GatewayServiceRestartExitCode):
		fmt.Fprintln(w, "  ⚠ Planned restart is stuck in systemd failed state (exit 75)")
		fmt.Fprintf(w, "  Run: systemctl %sreset-failed %s && %sicebath gateway start%s\n", userFlag, svc, sudo, scopeFlag)
	case activeState == "failed" && resultCode != "":
		fmt.Fprintf(w, "  ⚠ Systemd unit result: %s\n", resultCode)
	}
	if system {
		fmt.Fprintln(w, "✓ System service starts at boot without requiring systemd linger")
	} else {
		lingerEnabled, lingerDetail := GetSystemdLingerStatus(ctx, "")
		switch {
		case lingerEnabled != nil && *lingerEnabled:
			fmt.Fprintln(w, "✓ Systemd linger is enabled (service survives logout)")
		case lingerEnabled != nil:
			fmt.Fprintln(w, "⚠ Systemd linger is disabled (gateway may stop when you log out)")
			fmt.Fprintln(w, "  Run: sudo loginctl enable-linger $USER")
		case deep:
			fmt.Fprintf(w, "⚠ Could not verify systemd linger (%s)\n", lingerDetail)
			fmt.Fprintln(w, "  If you want the gateway user service to survive logout, run:")
			fmt.Fprintln(w, "  sudo loginctl enable-linger $USER")
		}
	}
	if deep {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Recent logs:")
		logCmd := []string{"journalctl"}
		if !system {
			logCmd = append(logCmd, "--user")
		}
		logCmd = append(logCmd, "-u", svc, "-n", "20", "--no-pager")
		if full {
			logCmd = append(logCmd, "-l")
		}
		return runServiceInherited(ctx, w, 10*time.Second, logCmd...)
	}
	return nil
}

// Prints the launchd plist freshness, load and supervision state and on deep the recent gateway log
func LaunchdStatus(ctx context.Context, w io.Writer, deep bool) error {
	plistPath, err := GetLaunchdPlistPath(ctx)
	if err != nil {
		return err
	}
	label, err := GetLaunchdLabel(ctx)
	if err != nil {
		return err
	}
	serviceListed, listOutput := false, ""
	result, err := RunServiceCommand(ctx, 10*time.Second, "launchctl", "list", label)
	switch {
	case errors.Is(err, ErrCommandTimeout):
	case err != nil:
		return err
	default:
		serviceListed, listOutput = result.ReturnCode == 0, result.Stdout
	}
	launchdPid, haveLaunchdPid := 0, false
	if serviceListed {
		launchdPid, haveLaunchdPid = ParseLaunchdPIDFromListOutput(listOutput)
	}
	fallbackPid, haveFallback, err := GetRunningPid("", false)
	if err != nil {
		return err
	}
	if haveLaunchdPid && haveFallback && fallbackPid == launchdPid {
		haveFallback = false
	}
	unsupported := LaunchdUnsupportedMarkerExists(ctx)
	fmt.Fprintf(w, "Launchd plist: %s\n", plistPath)
	current, err := LaunchdPlistIsCurrent(ctx)
	if err != nil {
		return err
	}
	if current {
		fmt.Fprintln(w, "✓ Service definition matches the current Icebath install")
	} else {
		fmt.Fprintln(w, "⚠ Service definition is stale relative to the current Icebath install")
		fmt.Fprintln(w, "  Run: icebath gateway start")
	}
	haveFallback = haveFallback && fallbackPid != 0
	switch {
	case !serviceListed:
		fmt.Fprintln(w, "✗ Gateway service is not loaded")
		fmt.Fprintln(w, "  Service definition exists locally but launchd has not loaded it.")
		fmt.Fprintln(w, "  Run: icebath gateway start")
		if haveFallback {
			fmt.Fprintf(w, "  Note: a detached gateway process is running (PID %d)\n", fallbackPid)
		}
	case haveLaunchdPid:
		fmt.Fprintf(w, "✓ Gateway is supervised by launchd (PID %d)\n", launchdPid)
		fmt.Fprintln(w, "  Auto-start at login and auto-restart on crash are available.")
		if unsupported {
			fmt.Fprintln(w, "  (launchd domain was previously unavailable but is now working)")
		}
	case unsupported:
		fmt.Fprintln(w, "⚠ Gateway service is registered but launchd is not supervising it")
		fmt.Fprintln(w, "  launchd cannot manage the gateway on this macOS version.")
		if haveFallback {
			fmt.Fprintf(w, "✓ Detached fallback process is running (PID %d)\n", fallbackPid)
			fmt.Fprintln(w, "  Cron jobs will fire. Stop with: icebath gateway stop")
		} else {
			fmt.Fprintln(w, "✗ No fallback process is running")
			fmt.Fprintln(w, "  Run: icebath gateway start")
		}
		fmt.Fprintln(w, "  ⚠ Auto-start at login and auto-restart on crash are NOT available.")
	default:
		fmt.Fprintln(w, "✓ Gateway service is registered with launchd")
		fmt.Fprintln(w, listOutput)
		if haveFallback {
			fmt.Fprintf(w, "  Detached gateway process is running (PID %d)\n", fallbackPid)
		}
	}
	if deep {
		logFile := pypath.PathJoin(home.Dir(ctx), "logs", "gateway.log")
		if pathExistsSafe(logFile) {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Recent logs:")
			return runServiceInherited(ctx, w, 10*time.Second, "tail", "-20", logFile)
		}
	}
	return nil
}

// Status, last run time and last run result of the scheduled task, empty when the query fails
func queryWindowsTaskStatus(ctx context.Context) (map[string]string, error) {
	taskName, err := windowsTaskName(ctx)
	if err != nil {
		return nil, err
	}
	code, out, _, err := winExecSchtasks([]string{"/Query", "/TN", taskName, "/V", "/FO", "LIST"})
	if err != nil {
		return nil, err
	}
	info := map[string]string{}
	if code != 0 {
		return info, nil
	}
	for _, raw := range pystr.Splitlines(out, false) {
		line := pystr.Strip(raw)
		if line == "" || !strings.Contains(line, ":") {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		key, value = pystr.Lower(pystr.Strip(key)), pystr.Strip(value)
		switch key {
		case "last result":
			if _, ok := info["last run result"]; !ok {
				info["last run result"] = value
			}
		case "status", "last run time", "last run result":
			info[key] = value
		}
	}
	return info, nil
}

// One PASS or FAIL deep probe row
func windowsProbe(w io.Writer, index int, ok bool, message string) {
	verdict := "FAIL"
	if ok {
		verdict = "PASS"
	}
	fmt.Fprintf(w, "  [%d] %-4s  %s\n", index, verdict, message)
}

// Reports a missing probed file as a failure, true when it was missing
func windowsProbeMissing(w io.Writer, index int, path, label string) bool {
	if pathExistsSafe(path) {
		return false
	}
	windowsProbe(w, index, false, fmt.Sprintf("%s missing: %s", label, path))
	return true
}

// Decoded JSON object of a utf-8-sig file, the str of whatever raised otherwise
func readJSONObjectSig(path string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(parsed))
	}
	return obj, nil
}

func windowsProbePidFile(w io.Writer, pidPath string) (int, bool) {
	if windowsProbeMissing(w, 1, pidPath, "PID file") {
		return 0, false
	}
	data, err := readJSONObjectSig(pidPath)
	pidValue, have := 0, false
	if err == nil {
		if raw := data.GetOr("pid", nil); raw != nil {
			n, convErr := pyIntArg(raw)
			if convErr != nil {
				err = convErr
			} else {
				pidValue, have = n, true
			}
		}
	}
	if err != nil {
		windowsProbe(w, 1, false, fmt.Sprintf("PID file present but unreadable: %s", err))
		return 0, false
	}
	shown := "None"
	if have {
		shown = strconv.Itoa(pidValue)
	}
	windowsProbe(w, 1, true, fmt.Sprintf("PID file present: %s (pid=%s)", pidPath, shown))
	return pidValue, have
}

// int of a pid record value the way int() converts it
func pyIntArg(v any) (int, error) {
	switch n := v.(type) {
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case string:
		parsed, err := strconv.Atoi(pystr.Strip(n))
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(n))
		}
		return parsed, nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))
}

func windowsProbeLockFile(w io.Writer, lockPath string) {
	if windowsProbeMissing(w, 2, lockPath, "Lock file") {
		return
	}
	active, err := IsGatewayRuntimeLockActive(lockPath)
	if err != nil {
		windowsProbe(w, 2, false, fmt.Sprintf("Could not probe lock: %s", err))
		return
	}
	windowsProbe(w, 2, active, fmt.Sprintf("Lock file held by a live process: %s", lockPath))
}

func windowsProbeRunningPid(w io.Writer) (int, bool) {
	pid, ok, err := GetRunningPid("", false)
	if err != nil {
		windowsProbe(w, 3, false, fmt.Sprintf("get_running_pid() raised: %s", pyrepr.ExceptionRepr(err)))
		return 0, false
	}
	shown := "None"
	if ok {
		shown = strconv.Itoa(pid)
	}
	windowsProbe(w, 3, ok, fmt.Sprintf("get_running_pid() => %s", shown))
	return pid, ok
}

func windowsProbePidExists(w io.Writer, pid int, have bool) {
	if !have {
		windowsProbe(w, 4, false, "No candidate PID to verify")
		return
	}
	alive := PidExists(pid)
	windowsProbe(w, 4, alive, fmt.Sprintf("_pid_exists(%d) => %s", pid, pyrepr.Repr(alive)))
}

func windowsProbeStateFile(w io.Writer, statePath string) {
	if windowsProbeMissing(w, 5, statePath, "gateway_state.json") {
		return
	}
	data, err := readJSONObjectSig(statePath)
	if err != nil {
		windowsProbe(w, 5, false, fmt.Sprintf("gateway_state.json present but unreadable: %s", err))
		return
	}
	gatewayState := data.GetOr("gateway_state", nil)
	ageStr := ""
	if updatedAt, ok := data.GetOr("updated_at", nil).(string); ok && updatedAt != "" {
		if stamp, aware, err := pydatetime.ParseISOFormat(updatedAt); err == nil && aware {
			ageStr = fmt.Sprintf(" (updated %ds ago)", int(time.Since(stamp).Seconds()))
		}
	}
	ok := gatewayState == "running" || gatewayState == "degraded"
	windowsProbe(w, 5, ok, fmt.Sprintf("gateway_state.json state=%s%s", pyrepr.Repr(gatewayState), ageStr))
}

func windowsProbeExitDiag(w io.Writer, diagPath string) {
	if windowsProbeMissing(w, 6, diagPath, "exit-diag log") {
		return
	}
	f, err := os.Open(diagPath)
	if err != nil {
		windowsProbe(w, 6, false, fmt.Sprintf("exit-diag log unreadable: %s", err))
		return
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err == nil {
		_, err = f.Seek(max(0, size-4096), io.SeekStart)
	}
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(f)
	}
	if err != nil {
		windowsProbe(w, 6, false, fmt.Sprintf("exit-diag log unreadable: %s", err))
		return
	}
	tail := pystr.Splitlines(pystr.DecodeUTF8Replace(raw), false)
	lastEvent := ""
	for i := len(tail) - 1; i >= 0; i-- {
		if pystr.Strip(tail[i]) != "" {
			lastEvent = tail[i]
			break
		}
	}
	if lastEvent == "" {
		windowsProbe(w, 6, false, fmt.Sprintf("exit-diag log empty: %s", diagPath))
		return
	}
	parsed, err := pyjson.Loads(lastEvent)
	event, isDict := parsed.(*pyjson.Object)
	if err != nil || !isDict {
		runes := []rune(lastEvent)
		windowsProbe(w, 6, false, fmt.Sprintf("Last lifecycle line not JSON: %s", string(runes[:min(len(runes), 120)])))
		return
	}
	tag := event.GetOr("tag", "?")
	windowsProbe(w, 6, tag == "gateway.start", fmt.Sprintf("Last lifecycle event: tag=%s pid=%s ts=%s",
		pyrepr.Str(tag), pyrepr.Str(event.GetOr("pid", "?")), pyrepr.Str(event.GetOr("ts", "?"))))
}

// Prints PASS or FAIL for each liveness signal the collapsed summary folds together
func printWindowsDeepProbes(ctx context.Context, w io.Writer) {
	h := winHome(ctx)
	fmt.Fprintln(w, "\nDeep probes:")
	pidValue, havePid := windowsProbePidFile(w, pypath.PathJoin(h, "gateway.pid"))
	windowsProbeLockFile(w, pypath.PathJoin(h, "gateway.lock"))
	runningPid, haveRunning := windowsProbeRunningPid(w)
	if haveRunning {
		windowsProbePidExists(w, runningPid, true)
	} else {
		windowsProbePidExists(w, pidValue, havePid)
	}
	windowsProbeStateFile(w, pypath.PathJoin(h, "gateway_state.json"))
	windowsProbeExitDiag(w, pypath.PathJoin(h, "logs", "gateway-exit-diag.log"))
}

// Prints the Windows scheduled task or login item state, the gateway process and on deep each liveness probe
func WindowsStatus(ctx context.Context, w io.Writer, deep bool) error {
	if err := assertWindows(); err != nil {
		return err
	}
	printStartAttestationWarning(ctx)
	taskName, err := windowsTaskName(ctx)
	if err != nil {
		return err
	}
	taskInstalled, err := windowsIsTaskRegistered(ctx)
	if err != nil {
		return err
	}
	startupInstalled, err := windowsStartupEntryInstalled(ctx)
	if err != nil {
		return err
	}
	pids, err := FindGatewayPIDsFor(ctx, nil, false)
	if err != nil {
		return err
	}
	switch {
	case taskInstalled:
		fmt.Fprintf(w, "✓ Scheduled Task registered: %s\n", taskName)
		info, err := queryWindowsTaskStatus(ctx)
		if err != nil {
			return err
		}
		for _, key := range []string{"status", "last run time", "last run result"} {
			if value, ok := info[key]; ok {
				fmt.Fprintf(w, "  %s: %s\n", pystr.Title(key), value)
			}
		}
		if err := printScheduledTaskDrift(ctx, taskName); err != nil {
			return err
		}
	case startupInstalled:
		entry, err := windowsStartupEntry(ctx, ".vbs")
		if err != nil {
			return err
		}
		if !pathExistsSafe(entry) {
			if entry, err = windowsStartupEntry(ctx, ".cmd"); err != nil {
				return err
			}
		}
		fmt.Fprintf(w, "✓ Windows login item installed: %s\n", entry)
	default:
		fmt.Fprintln(w, "✗ Gateway service not installed")
	}
	if _, err := WarnLegacyLaunchers(ctx); err != nil {
		return err
	}
	if len(pids) > 0 {
		parts := make([]string, len(pids))
		for i, p := range pids {
			parts[i] = strconv.Itoa(p)
		}
		fmt.Fprintf(w, "✓ Gateway process running (PID: %s)\n", strings.Join(parts, ", "))
	} else {
		fmt.Fprintln(w, "✗ No gateway process detected")
	}
	if deep {
		scriptPath, err := windowsTaskScriptPath(ctx)
		if err != nil {
			return err
		}
		entry, err := windowsStartupEntry(ctx, ".vbs")
		if err != nil {
			return err
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  Task name:        %s\n", taskName)
		fmt.Fprintf(w, "  Task script:      %s\n", scriptPath)
		fmt.Fprintf(w, "  Startup entry:    %s\n", entry)
		printWindowsDeepProbes(ctx, w)
	}
	if !taskInstalled && !startupInstalled && len(pids) == 0 {
		fmt.Fprintln(w, "\nTo install:\n  icebath gateway install")
	}
	return nil
}
