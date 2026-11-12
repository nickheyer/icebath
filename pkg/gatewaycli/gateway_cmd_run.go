package gatewaycli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Spawn metadata a Windows launcher sets on a breakaway gateway child
const windowsGatewayBreakawayEnv = "_ICEBATH_GATEWAY_BREAKAWAY"

// Environment flag value read the truthy way, 1 true yes or on
func truthyEnv(name string) bool {
	switch pystr.Lower(pystr.Strip(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func gatewayDetachedEnv() bool { return truthyEnv("ICEBATH_GATEWAY_DETACHED") }

// Whether this process is the supervisor launched gateway, so the conflict guards never wedge it
func runningUnderGatewaySupervisor() bool { return gateway.IsGatewaySupervisorProcess(nil) }

// Whether stdin is a terminal, nil when stdin is closed or invalid
func stdinIsTTY() *bool {
	if os.Stdin == nil {
		return nil
	}
	tty := gatewayConsole(io.Discard).StdinIsTTY()
	return &tty
}

// True for a detached Windows gateway run that should ignore Ctrl+C
func windowsGatewayShouldAbsorbConsoleControls() bool {
	if !gateway.IsWindows() {
		return false
	}
	if gatewayDetachedEnv() {
		return true
	}
	tty := stdinIsTTY()
	return tty == nil || !*tty
}

// Breakaway metadata a Windows launcher left in the environment, consumed once, nil when absent or unknown
func windowsGatewayBreakawayState() *bool {
	if !gateway.IsWindows() {
		return nil
	}
	raw, ok := os.LookupEnv(windowsGatewayBreakawayEnv)
	os.Unsetenv(windowsGatewayBreakawayEnv)
	if !ok {
		return nil
	}
	switch raw {
	case "1":
		t := true
		return &t
	case "0":
		f := false
		return &f
	}
	return nil
}

// Whether this checkout is the official Docker image install
func isOfficialDockerCheckout() bool {
	root := pm.RepoRoot()
	if root != "/opt/icebath" {
		return false
	}
	st, err := os.Stat(filepath.Join(root, "docker", "entrypoint.sh"))
	return err == nil && st.Mode().IsRegular()
}

// Exit code of a host attach verdict, 75 for a transient runtime refusal and 78 for a permanent one
func hostDecisionExitCode(decision gateway.HostAttachDecision) int {
	if decision.Transient {
		return gateway.GatewayServiceRestartExitCode
	}
	return gateway.GatewayFatalConfigExitCode
}

// Attaches to, replaces or refuses the one host gateway before a foreground run
func attachToHostGatewayOrGuard(ctx context.Context, w io.Writer, force, replace bool) error {
	if force {
		return nil
	}
	decision, err := gateway.DecideHostAttach(ctx, home.Dir(ctx), replace)
	decided := err == nil
	if err != nil {
		slog.Debug("Host gateway attach probe failed", "error", err)
	}
	if decided && decision.Outcome == gateway.HostAttachReplaceHost {
		return nil
	}
	if decided && (decision.Outcome == gateway.HostAttachAttach || decision.Outcome == gateway.HostAttachRefuse) {
		fmt.Fprintln(w, decision.Message)
		if decision.Outcome == gateway.HostAttachRefuse {
			code := hostDecisionExitCode(decision)
			slog.Warn(fmt.Sprintf("gateway run refused (exit %d): %s", code, decision.Message))
			return exitWith(code)
		}
		if runningUnderGatewaySupervisor() {
			return exitWith(hostDecisionExitCode(decision))
		}
		return exitWith(0)
	}
	return guardNamedProfileUnderMultiplexer(ctx, w, force)
}

// Refuses a foreground gateway while a service manager already supervises one for this profile
func guardSupervisedGatewayConflict(ctx context.Context, w io.Writer, force bool) error {
	if force || runningUnderGatewaySupervisor() {
		return nil
	}
	installed, err := gateway.IsServiceInstalled(ctx)
	if err != nil || !installed {
		return err
	}
	snapshot, err := GetGatewayRuntimeSnapshot(ctx, false)
	if err != nil {
		slog.Debug("Supervised-gateway conflict probe failed", "error", err)
		return nil
	}
	if !(snapshot.ServiceInstalled && snapshot.ServiceRunning) {
		return nil
	}
	gatewayConsole(w).PrintError(fmt.Sprintf("A gateway is already running under %s for this profile.", snapshot.Manager))
	printLines(w, "  Starting another one from a shell leaves an orphan dispatcher that\n"+
		"  escapes the service, survives restarts, and writes to the same kanban\n"+
		"  DB concurrently — which can corrupt it. Restart the supervised gateway\n"+
		"  instead:", "", "    icebath gateway restart", "",
		"  Pass --force to start a foreground gateway anyway (not recommended\n"+
			"  while the service is running).")
	return exitWith(1)
}

// Pid file preflight refusing a second gateway before the runner's authoritative lock check
func guardExistingGatewayProcessConflict(w io.Writer, replace bool) error {
	if replace || runningUnderGatewaySupervisor() {
		return nil
	}
	pid, ok, err := gateway.GetRunningPid("", true)
	if err != nil {
		slog.Debug("Existing-gateway process probe failed", "error", err)
		return nil
	}
	if !ok {
		if stale := gateway.ReadPidRecord(""); stale != nil {
			if belongs, err := gateway.PidRecordBelongsToCurrentProfile(stale); err == nil && !belongs {
				slog.Warn(fmt.Sprintf("PID file belongs to another profile (icebath_home=%s). "+
					"The old gateway may still be running under that profile.", stale.GetOr("hermes_home", "<unknown>")))
			}
		}
		return nil
	}
	gatewayConsole(w).PrintError(fmt.Sprintf("A gateway is already running (PID %d), so your bots are most likely online already.", pid))
	printLines(w, "  Check with `icebath gateway status`.",
		"  To restart it: `icebath gateway restart`. To stop it: `icebath gateway stop`.",
		"  To replace it from here: `icebath gateway run --replace`.")
	return exitWith(1)
}

// Refuses a root gateway inside the official Docker image when its privilege drop was bypassed
func guardOfficialDockerRootGateway(w io.Writer) error {
	if runtime.GOOS == "windows" || os.Geteuid() != 0 || truthyEnv("ICEBATH_ALLOW_ROOT_GATEWAY") || !isOfficialDockerCheckout() {
		return nil
	}
	gatewayConsole(w).PrintError("Refusing to run the Icebath gateway as root inside the official Docker image.")
	printLines(w, "  The image entrypoint normally drops privileges to the 'icebath' user. "+
		"If you override entrypoint in Docker Compose, include "+
		"/opt/icebath/docker/entrypoint.sh before the Icebath command.",
		"  Running the gateway as root can leave root-owned files in "+
			"$ICEBATH_HOME and break later non-root dashboard/gateway runs.",
		"  Set ICEBATH_ALLOW_ROOT_GATEWAY=1 only if you intentionally accept this risk.")
	return exitWith(1)
}

// Arms the startup liveness watchdog after the conflict guards, bridging the gateway config into its env knobs
func applyStartupWatchdogConfig(ctx context.Context) {
	timeoutBridged := false
	if cfg, err := config.Load(home.Dir(ctx), nil); err == nil {
		gw, _ := cfg["gateway"].(map[string]any)
		if _, set := os.LookupEnv(gateway.EnvStartupWatchdog); !set {
			if enabled, present := gw["startup_watchdog"]; present && !pyrepr.Truthy(enabled) {
				os.Setenv(gateway.EnvStartupWatchdog, "0")
			}
		}
		if timeout, present := gw["startup_watchdog_timeout_seconds"]; present && timeout != nil {
			if _, set := os.LookupEnv(gateway.EnvStartupWatchdogTimeoutS); !set {
				os.Setenv(gateway.EnvStartupWatchdogTimeoutS, pyrepr.Str(timeout))
				timeoutBridged = true
			}
		}
	}
	if gateway.StartupWatchdogDisabled() {
		gateway.DisarmStartupWatchdog()
		return
	}
	if timeoutBridged {
		gateway.DisarmStartupWatchdog()
	}
	gateway.ArmStartupWatchdog(ctx)
}

// Respawn storm breaker for supervisors without a restart floor, never blocking startup on its own failure
func respawnStormBackoff(ctx context.Context) {
	maxStarts, window := 5, 120.0
	if cfg, err := config.Load(home.Dir(ctx), nil); err == nil {
		if gw, ok := cfg["gateway"].(map[string]any); ok {
			if rs, ok := gw["respawn_storm"].(map[string]any); ok {
				if n, isInt := pyIntExact(rs["max_starts"]); isInt {
					maxStarts = n
				}
				if f, isNum := pyNumber(rs["window_seconds"]); isNum {
					window = f
				}
			}
		}
	}
	if raw, ok := os.LookupEnv("ICEBATH_GATEWAY_MAX_STARTS"); ok {
		if n, ok := pyint.Parse(pystr.Strip(raw)); ok {
			maxStarts = int(n.Int64())
		}
	}
	if raw, ok := os.LookupEnv("ICEBATH_GATEWAY_START_WINDOW_S"); ok {
		if f, err := pyint.ParseFloat(raw); err == nil {
			window = f
		}
	}
	if maxStarts <= 0 {
		return
	}
	storm := gateway.RecordStartAndCheckStorm(ctx, home.Process(), maxStarts, window, 300.0)
	if storm == nil {
		return
	}
	slog.Warn(fmt.Sprintf("Gateway (re)started %d times in %.0fs — backing off %.0fs to break a respawn storm.", storm.Count, storm.WindowS, storm.BackoffS))
	gateway.KickStartupWatchdog(storm.BackoffS)
	time.Sleep(time.Duration(storm.BackoffS * float64(time.Second)))
}

// Recorder of every way the runner returns, one JSON line per event in logs/gateway-exit-diag.log
func makeExitDiag(ctx context.Context) func(tag string, extra ...[2]any) {
	return func(tag string, extra ...[2]any) {
		if v, ok := os.LookupEnv("ICEBATH_GATEWAY_EXIT_DIAG"); ok && v != "1" {
			return
		}
		logDir := filepath.Join(home.Dir(ctx), "logs")
		if err := os.MkdirAll(logDir, 0o777); err != nil {
			return
		}
		line := pyjson.NewObject()
		line.Set("ts", pydatetime.ISOFormatUTC(time.Now()))
		line.Set("tag", tag)
		line.Set("pid", int64(os.Getpid()))
		line.Set("go", hostinfo.GoVersion())
		line.Set("platform", pythonPlatform())
		for _, kv := range extra {
			line.Set(kv[0].(string), kv[1])
		}
		text, err := pyjson.Dumps(line, pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
		if err != nil {
			return
		}
		f, err := os.OpenFile(filepath.Join(logDir, "gateway-exit-diag.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(text + "\n")
	}
}

// sys.platform spelling of the host OS
func pythonPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	}
	return runtime.GOOS
}

// Runs the gateway in the foreground until it exits, then hard exits with its status
func runGateway(ctx context.Context, w io.Writer, verbose int, quiet, replace, force, idleExit bool) error {
	if err := guardOfficialDockerRootGateway(w); err != nil {
		return err
	}
	if err := attachToHostGatewayOrGuard(ctx, w, force, replace); err != nil {
		return err
	}
	if err := guardSupervisedGatewayConflict(ctx, w, force); err != nil {
		return err
	}
	if err := guardExistingGatewayProcessConflict(w, replace); err != nil {
		return err
	}
	applyStartupWatchdogConfig(ctx)
	observability.BeginProcess(ctx, "gateway")
	stdinState := stdinIsTTY()
	stdinTTY := stdinState != nil && *stdinState
	consoleAttached := windowsConsoleWindowAttached()
	breakaway := windowsGatewayBreakawayState()
	absorb := windowsGatewayShouldAbsorbConsoleControls()
	if absorb {
		absorbWindowsConsoleControls()
	}
	if gateway.IsLinux() && os.Getenv("INVOCATION_ID") != "" {
		gateway.EnsureUserSystemdEnv()
	}
	if gateway.SupportsSystemdServices(ctx) {
		_, _ = gateway.RefreshSystemdUnitIfNeeded(ctx, false)
	}
	if gatewayDetachedEnv() {
		if err := logging.RedactDetachedStdio(); err != nil {
			return err
		}
	}
	if err := gatewayrun.ImportRunModule(ctx); err != nil {
		return err
	}
	printLines(w, "┌─────────────────────────────────────────────────────────┐",
		"│           ☤ Icebath Gateway Starting...                 │",
		"├─────────────────────────────────────────────────────────┤",
		"│  Messaging platforms + cron scheduler                    │",
		"│  Press Ctrl+C to stop                                   │",
		"└─────────────────────────────────────────────────────────┘", "")
	var verbosity *int
	if !quiet {
		verbosity = &verbose
	}
	exitDiag := makeExitDiag(ctx)
	argv := make([]any, len(os.Args))
	for i, arg := range os.Args {
		argv[i] = arg
	}
	exitDiag("gateway.start", [2]any{"replace", replace}, [2]any{"argv", argv}, [2]any{"stdin_is_tty", stdinTTY},
		[2]any{"console_window_attached", optionalFlag(consoleAttached)}, [2]any{"detached", gatewayDetachedEnv()},
		[2]any{"breakaway", optionalFlag(breakaway)}, [2]any{"absorb_windows_console_controls", absorb})
	gatewayrun.RegisterAtExit(func() { exitDiag("atexit.hook", [2]any{"sys_exc", "(None, None, None)"}) })
	respawnStormBackoff(ctx)
	success, err := gatewayrun.RunGateway(ctx, gatewayrun.StartGatewayOptions{Replace: replace, Force: force, Verbosity: verbosity, IdleExit: idleExit})
	var exit *gateway.SystemExitError
	switch {
	case errors.Is(err, gatewayrun.ErrKeyboardInterrupt):
		exitDiag("asyncio.run.KeyboardInterrupt", [2]any{"traceback", "Traceback (most recent call last):\nKeyboardInterrupt\n"})
		fmt.Fprintln(w, "\nGateway stopped.")
		gatewayrun.ExitAfterGracefulShutdown(ctx, 0)
		return nil
	case errors.As(err, &exit):
		exitDiag("asyncio.run.SystemExit", [2]any{"code", exit.Code}, [2]any{"traceback", fmt.Sprintf("Traceback (most recent call last):\nSystemExit: %d\n", exit.Code)})
		gatewayrun.ExitAfterGracefulShutdown(ctx, exit.Code)
		return nil
	case err != nil:
		exitDiag("asyncio.run.exception", [2]any{"exc_type", pyrepr.ExceptionName(err)}, [2]any{"exc_repr", pyrepr.ExceptionRepr(err)},
			[2]any{"traceback", fmt.Sprintf("Traceback (most recent call last):\n%s: %s\n", pyrepr.ExceptionName(err), pyrepr.ExceptionMessage(err))})
		return err
	}
	exitDiag("asyncio.run.returned", [2]any{"success", success})
	if !success {
		exitDiag("gateway.exit_nonzero")
		gatewayrun.ExitAfterGracefulShutdown(ctx, 1)
		return nil
	}
	exitDiag("gateway.exit_clean")
	gatewayrun.ExitAfterGracefulShutdown(ctx, 0)
	return nil
}

// Upgrades a bare gateway run inside an s6 container to the supervised longrun, true when dispatched
func maybeRedirectRunToS6Supervision(ctx context.Context, w, errw io.Writer, ns map[string]any) (bool, error) {
	noSupervise := nsFlag(ns, "no_supervise")
	switch pystr.Lower(os.Getenv("ICEBATH_GATEWAY_NO_SUPERVISE")) {
	case "1", "true", "yes":
		noSupervise = true
	}
	if noSupervise || os.Getenv("ICEBATH_S6_SUPERVISED_CHILD") != "" {
		return false, nil
	}
	dispatched, err := dispatchViaServiceManagerIfS6(ctx, w, "start", "")
	if err != nil || !dispatched {
		return false, err
	}
	gateway.DisarmStartupWatchdog()
	fmt.Fprintln(errw, "→ gateway is now running under s6 supervision (auto-restart on crash,\n"+
		"  dashboard supervised alongside if ICEBATH_DASHBOARD is set).\n"+
		"  This is the recommended setup for the s6 container image — the\n"+
		"  gateway will keep running even if it crashes.\n"+
		"  Use `--no-supervise` (or ICEBATH_GATEWAY_NO_SUPERVISE=1) to opt out\n"+
		"  and get the pre-s6 foreground behavior instead.")
	if sleep, err := exec.LookPath("sleep"); err == nil {
		err = syscall.Exec(sleep, []string{"sleep", "infinity"}, os.Environ())
		if err == nil {
			return true, nil
		}
	}
	fmt.Fprintln(errw, "→ `sleep` is unavailable; keeping the s6 CMD process alive in-process until the container is stopped.")
	return true, blockUntilTerminated()
}

// Heartbeat when sleep cannot exec, SIGTERM exiting 128 plus the signal number
func blockUntilTerminated() error {
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	sig := <-terms
	return exitWith(128 + int(sig.(syscall.Signal)))
}

func cmdRun(ctx context.Context, w, errw io.Writer, ns map[string]any) error {
	if redirected, err := maybeRedirectRunToS6Supervision(ctx, w, errw, ns); err != nil || redirected {
		return err
	}
	if nsFlag(ns, "external_supervisor") {
		os.Setenv(gateway.ExternalGatewaySupervisorEnv, "1")
	}
	verbose, _ := ns["verbose"].(int)
	return runGateway(ctx, w, verbose, nsFlag(ns, "quiet"), nsFlag(ns, "replace"), nsFlag(ns, "force"), nsFlag(ns, "idle_exit"))
}

// Starts the manual fallback's replacement, detached for a bounded caller and in the foreground otherwise
func startRestartReplacement(ctx context.Context, w io.Writer, ns map[string]any, force bool) error {
	if nsFlag(ns, "detach") {
		return exitWith(gateway.StartDetachedReplacement(ctx, home.Dir(ctx), force, w, os.Stderr))
	}
	return runGateway(ctx, w, 0, false, false, force, false)
}

// Restarts the one host multiplexer, refusing when another profile owns it
func restartAll(ctx context.Context, w io.Writer, system, detach bool) error {
	owner := hostMultiplexerForAllVerb()
	if owner != nil && !hostMultiplexerIsOurs(owner) {
		gatewayConsole(w).PrintError(fmt.Sprintf("The host gateway runs under profile '%s'.", owner.ProfileLabel()))
		fmt.Fprintf(w, "  %s\n", owner.Describe())
		fmt.Fprintln(w, "  `--all` restarts the one host multiplexer, and this profile is not its owner.")
		fmt.Fprintln(w)
		fmt.Fprintf(w, "    icebath -p %s gateway restart --all\n", owner.ProfileLabel())
		return exitWith(gateway.GatewayFatalConfigExitCode)
	}
	scoped, restore := hostScopeForAllVerb(ctx, owner)
	defer restore()
	return restartAllAsHost(scoped, w, owner, system, detach, func() { gateway.WritePlannedRestartMarker(scoped, false, false) })
}

func restartAllAsHost(ctx context.Context, w io.Writer, owner *gateway.HostGateway, system, detach bool, oweOnlineNotice func()) error {
	serviceStopped, err := stopInstalledService(ctx, system)
	if err != nil {
		return err
	}
	if owner != nil {
		stopped, err := stopHostMultiplexer(ctx, w, owner)
		if err != nil {
			return err
		}
		if serviceStopped {
			stopped++
		}
		if stopped > 0 {
			served := strings.Join(owner.Profiles, ", ")
			if served == "" {
				served = "unknown"
			}
			fmt.Fprintf(w, "✓ Stopped the host gateway (PID %d; serves %s)\n", owner.PID, served)
		}
		printUnfoldedGatewayNote(ctx, w, owner)
	} else {
		total, err := gateway.KillGatewayProcesses(ctx, w, false, nil, true)
		if err != nil {
			return err
		}
		if serviceStopped {
			total++
		}
		if total > 0 {
			fmt.Fprintf(w, "✓ Stopped %d gateway process(es) across all profiles\n", total)
		}
	}
	gateway.WaitForGatewayExitForcing(10.0, 5.0)
	if _, err := gateway.WaitForAPIServerPortFree(ctx, 10.0); err != nil {
		return err
	}
	gateway.DiscardDeadHostRecord(gateway.RoleGateway)
	gateway.InvalidateHostGatewayCache()
	oweOnlineNotice()
	fmt.Fprintln(w, "Starting gateway...")
	kind, err := gateway.InstalledServiceKindFor(ctx, func() (bool, error) { return gateway.IsWindows(), nil })
	if err != nil {
		return err
	}
	if kind != "" {
		return gateway.ServiceCall(ctx, kind, "start", system)
	}
	if detach || home.NamedProfileHome(home.RoutingProcess()) != "" {
		spawned, err := gateway.SpawnDetachedGateway(ctx)
		if err != nil {
			return err
		}
		if !spawned {
			gatewayConsole(w).PrintError("Failed to start the host gateway as a background process.")
			return exitWith(1)
		}
		fmt.Fprintln(w, "✓ Started the host gateway as a background process")
		return nil
	}
	return runGateway(ctx, w, 0, false, true, false, false)
}

func cmdRestart(ctx context.Context, w io.Writer, ns map[string]any) error {
	if err := refuseFromInsideGateway(w, "restart", "restart loops"); err != nil {
		return err
	}
	if handled, err := ProfileLifecycle(ctx, w, "restart", ns); err != nil || handled {
		return err
	}
	system, all, force := nsFlag(ns, "system"), nsFlag(ns, "all"), nsFlag(ns, "force")
	if !all {
		if err := guardNamedProfileUnderMultiplexer(ctx, w, force); err != nil {
			return err
		}
	}
	if err := RefuseWhileSessionsLive(ctx, w, "restart", ns, "", ""); err != nil {
		return err
	}
	if all {
		if dispatched, err := dispatchAllViaServiceManagerIfS6(ctx, w, "restart"); err != nil || dispatched {
			return err
		}
		return restartAll(ctx, w, system, nsFlag(ns, "detach"))
	}
	if dispatched, err := dispatchViaServiceManagerIfS6(ctx, w, "restart", ""); err != nil || dispatched {
		return err
	}
	if _, err := gateway.ReapUnsupervisedGatewayOrphans(ctx, nil, 0); err != nil {
		slog.Debug(fmt.Sprintf("orphan reap before gateway restart failed: %s", err))
	}
	kind, err := gateway.InstalledServiceKindFor(ctx, func() (bool, error) { return gateway.IsWindows(), nil })
	if err != nil {
		return err
	}
	serviceConfigured := kind != ""
	if kind == "windows" {
		if serviceConfigured, err = gateway.WindowsIsInstalled(ctx); err != nil {
			return err
		}
	}
	if kind != "" {
		err := gateway.ServiceCall(ctx, kind, "restart", system)
		var exit *gateway.SystemExitError
		switch {
		case err == nil:
			return nil
		case isCalledProcessError(err):
		case kind == "windows" && !errors.As(err, &exit):
		default:
			return err
		}
	}
	if kind == "systemd" && gateway.SupportsSystemdServices(ctx) {
		if lingerOK, _ := gateway.GetSystemdLingerStatus(ctx, ""); lingerOK == nil || !*lingerOK {
			user := os.Getenv("USER")
			printLines(w, "", "⚠ Cannot restart gateway as a service — linger is not enabled.",
				"  The gateway user service requires linger to function on headless servers.", "",
				"  Run:  sudo loginctl enable-linger "+user, "", "  Then restart the gateway:", "    icebath gateway restart")
			return nil
		}
	}
	if serviceConfigured {
		printLines(w, "", "✗ Gateway service restart failed.",
			"  The service definition exists, but the service manager did not recover it.",
			"  Fix the service, then retry: icebath gateway start")
		return exitWith(1)
	}
	supervisedPid, live, err := gateway.GetRunningPid("", true)
	if err != nil {
		return err
	}
	if live && supervisedPid != 0 && GatewayDeclaresExternalSupervisor(supervisedPid, "") {
		return RestartExternallySupervisedGateway(ctx, w, supervisedPid)
	}
	if err := RefuseRestartOfServiceManagedGateway(ctx, w, supervisedPid); err != nil {
		return err
	}
	stopped, err := gateway.StopProfileGateway(ctx)
	if err != nil {
		return err
	}
	if stopped {
		fmt.Fprintln(w, "✓ Stopped gateway for this profile")
	}
	gateway.WaitForGatewayExitForcing(10.0, 5.0)
	if _, err := gateway.WaitForAPIServerPortFree(ctx, 10.0); err != nil {
		return err
	}
	gateway.InvalidateHostGatewayCache()
	fmt.Fprintln(w, "Starting gateway...")
	return startRestartReplacement(ctx, w, ns, force)
}

// Dispatches one gateway subcommand by its argparse name, bare gateway running in the foreground
func gatewayCommandInner(ctx context.Context, ns map[string]any, w, errw io.Writer) error {
	sub, _ := ns["gateway_command"].(string)
	switch sub {
	case "", "run":
		return cmdRun(ctx, w, errw, ns)
	case "setup":
		return cmdSetup(ctx, w)
	case "install":
		return cmdInstall(ctx, w, ns)
	case "uninstall":
		return cmdUninstall(ctx, w, ns)
	case "start":
		return cmdStart(ctx, w, ns)
	case "stop":
		return cmdStop(ctx, w, ns)
	case "restart":
		return cmdRestart(ctx, w, ns)
	case "status":
		return cmdStatus(ctx, w, ns)
	case "list":
		return cmdList(ctx, w)
	case "migrate-legacy":
		return cmdMigrateLegacy(ctx, w, ns)
	case "migrate":
		return gatewayrun.CmdMigrate(ctx, w, ns)
	}
	return nil
}

// Handles the gateway subcommands, systemd failures ending as guidance instead of a traceback
func GatewayCommand(ctx context.Context, ns map[string]any, w, errw io.Writer) (int, error) {
	return gatewayCommandExit(w, gatewayCommandInner(ctx, ns, w, errw))
}

// Value of a config entry Python counts as an int, a bool included as one or zero
func pyIntExact(v any) (int, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// Float of a config entry Python counts as an int or a float
func pyNumber(v any) (float64, bool) {
	if f, isFloat := v.(float64); isFloat {
		return f, true
	}
	n, isInt := pyIntExact(v)
	return float64(n), isInt
}

// JSON value of an optional flag, null when unknown
func optionalFlag(flag *bool) any {
	if flag == nil {
		return nil
	}
	return *flag
}
