package gatewaycli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Steward owning the sealed install tree, installed by cmd/icebath over the self update steward probe
var SealedSteward func(projectRoot string) *string

// Steward stamp of a Termux APT package install
const stewardAptTermux = "apt-termux"

var wslForegroundHint = []string{"", "  icebath gateway run                              # direct foreground",
	"  tmux new -s icebath 'icebath gateway run'         # persistent via tmux",
	"  nohup icebath gateway run > ~/.icebath/logs/gateway.log 2>&1 &  # background"}

// Exit code and lines of a subcommand with no service backend, a nil code returning plainly
type noBackendMessage struct {
	code  *int
	lines []string
}

func exitCode(n int) *int { return &n }

func joinLines(parts ...[]string) []string {
	out := []string{}
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var noBackendMessages = map[[2]string]noBackendMessage{
	{"install", "termux"}: {exitCode(1), []string{"Gateway service installation is not supported on Termux.", "Run manually: icebath gateway"}},
	{"install", "wsl"}: {exitCode(1), joinLines([]string{"WSL detected but systemd is not running.",
		"Either enable systemd (add systemd=true to /etc/wsl.conf and restart WSL)",
		"or run the gateway in foreground mode:"}, wslForegroundHint)},
	{"install", "s6"}: {nil, []string{"Per-profile gateways are auto-registered when you create a profile.", "",
		"  icebath profile create <name>     # creates the s6 service slot",
		"  icebath -p <name> gateway start   # bring it up via s6",
		"  icebath status                    # see currently-supervised gateways"}},
	{"install", "container"}: {exitCode(0), []string{"Service installation is not needed inside a Docker container.",
		"The container runtime is your service manager — use Docker restart policies instead:", "",
		"  docker run --restart unless-stopped ...   # auto-restart on crash/reboot",
		"  docker restart <container>                # manual restart", "",
		"To run the gateway: icebath gateway run"}},
	{"install", "unsupported"}: {exitCode(1), []string{"Service installation not supported on this platform.", "Run manually: icebath gateway run"}},
	{"uninstall", "termux"}: {exitCode(1), []string{"Gateway service uninstall is not supported on Termux because there is no managed service to remove.",
		"Stop manual runs with: icebath gateway stop"}},
	{"uninstall", "s6"}: {nil, []string{"Per-profile gateways are auto-unregistered when you delete the profile.", "",
		"  icebath profile delete <name>     # tears down the s6 service slot",
		"  icebath -p <name> gateway stop    # stop without deleting the profile"}},
	{"uninstall", "container"}: {exitCode(0), []string{"Service uninstall is not applicable inside a Docker container.",
		"To stop the gateway, stop or remove the container:", "", "  docker stop <container>", "  docker rm <container>"}},
	{"uninstall", "unsupported"}: {exitCode(1), []string{"Running the gateway as a background service is not available on this platform " +
		"(no systemd, launchd or Scheduled Tasks), so there is nothing to uninstall.",
		"Stop a manually started gateway with: icebath gateway stop"}},
	{"start", "termux"}: {exitCode(1), []string{"Gateway service start is not supported on Termux because there is no system service manager.",
		"Run manually: icebath gateway"}},
	{"start", "wsl"}: {exitCode(1), joinLines([]string{"WSL detected but systemd is not available.",
		"Run the gateway in foreground mode instead:"}, wslForegroundHint,
		[]string{"", "To enable systemd: add systemd=true to /etc/wsl.conf and run 'wsl --shutdown' from PowerShell."})},
	{"start", "container"}: {exitCode(0), []string{"Service start is not applicable inside a Docker container.",
		"The gateway runs as the container's main process.", "",
		"  docker start <container>     # start a stopped container",
		"  docker restart <container>   # restart a running container", "",
		"Or run the gateway directly: icebath gateway run"}},
	{"start", "unsupported"}: {exitCode(1), []string{"Running the gateway as a background service is not available on this platform " +
		"(no systemd, launchd or Scheduled Tasks).", "Run it directly with: icebath gateway run"}},
}

// Prints the no backend lines of the subcommand and reason, exiting with their code when one is set
func noBackendExit(w io.Writer, subcommand, reason string) error {
	msg := noBackendMessages[[2]string{subcommand, reason}]
	printLines(w, msg.lines...)
	if msg.code != nil {
		return exitWith(*msg.code)
	}
	return nil
}

// Whether this install is a sealed Termux APT package tree, keyed on its steward stamp
func isAptTermuxInstall() bool {
	steward := SealedSteward(pm.RepoRoot())
	return steward != nil && *steward == stewardAptTermux
}

// Whether service lanes are unavailable, a Termux device or a sealed apt termux install
func serviceMgmtBlocked() bool { return hostinfo.IsTermux() || isAptTermuxInstall() }

// Fallthrough when no service backend matched, WSL then container then unsupported
func handleNoBackend(w io.Writer, subcommand string, wsl, s6 bool) error {
	reason := "unsupported"
	switch {
	case wsl && hostinfo.IsWSL():
		reason = "wsl"
	case hostinfo.IsContainer():
		reason = "container"
		if s6 && runningUnderS6() {
			reason = "s6"
		}
	}
	return noBackendExit(w, subcommand, reason)
}

func runningUnderS6() bool {
	kind, err := DetectServiceManager()
	return err == nil && kind == "s6"
}

// Dispatches start, stop or restart through s6 inside an s6 container, true when dispatched
func dispatchViaServiceManagerIfS6(ctx context.Context, w io.Writer, action, profile string) (bool, error) {
	if !runningUnderS6() {
		return false, nil
	}
	if profile == "" {
		name, err := gateway.CurrentProfileName(ctx)
		if err != nil {
			return false, err
		}
		profile = name
	}
	mgr, err := GetServiceManager()
	if err != nil {
		return false, err
	}
	if action != "start" && action != "stop" && action != "restart" {
		return false, nil
	}
	service := S6ServicePrefix + profile
	run := func() error {
		switch action {
		case "start":
			return mgr.Start(service)
		case "stop":
			return mgr.Stop(ctx, service)
		}
		return mgr.Restart(service)
	}
	err = run()
	var notRegistered *GatewayNotRegisteredError
	if errors.As(err, &notRegistered) && action == "start" {
		registered, regErr := RegisterUnregisteredProfileGateway(mgr, profile)
		switch {
		case regErr != nil:
			err = regErr
		case registered:
			fmt.Fprintf(w, "✓ registered the s6 gateway slot for profile %s\n", pyrepr.Repr(profile))
			err = mgr.Start(service)
		}
	}
	if err != nil {
		fmt.Fprintf(w, "✗ %s\n", err)
		return false, exitWith(1)
	}
	return true, nil
}

// Dispatches an all stop or restart to every registered s6 profile gateway, true when dispatched
func dispatchAllViaServiceManagerIfS6(ctx context.Context, w io.Writer, action string) (bool, error) {
	if !runningUnderS6() || (action != "stop" && action != "restart") {
		return false, nil
	}
	mgr, err := GetServiceManager()
	if err != nil {
		return false, err
	}
	names, err := mgr.ListProfileGateways()
	if err != nil {
		return false, err
	}
	if len(names) == 0 {
		fmt.Fprintln(w, "✗ No profile gateways registered under s6")
		return true, nil
	}
	type failure struct {
		profile string
		err     error
	}
	failures := []failure{}
	for _, profile := range names {
		var err error
		if action == "stop" {
			err = mgr.Stop(ctx, S6ServicePrefix+profile)
		} else {
			err = mgr.Restart(S6ServicePrefix + profile)
		}
		if err != nil {
			failures = append(failures, failure{profile, err})
		}
	}
	verb := "restarted"
	if action == "stop" {
		verb = "stopped"
	}
	if succeeded := len(names) - len(failures); succeeded > 0 {
		fmt.Fprintf(w, "✓ %s %d profile gateway(s) under s6\n", pystr.Capitalize(verb), succeeded)
	}
	for _, f := range failures {
		fmt.Fprintf(w, "✗ Could not %s gateway-%s: %s\n", action, f.profile, f.err)
	}
	return true, nil
}

// Installed service kind, stricter than the backend since the unit, plist or task must exist
func installedServiceKind(ctx context.Context) (string, error) {
	return gateway.InstalledServiceKindFor(ctx, func() (bool, error) {
		if !gateway.IsWindows() {
			return false, nil
		}
		return gateway.WindowsIsInstalled(ctx)
	})
}

// Whether err is a checked subprocess failure
func isCalledProcessError(err error) bool {
	var gwErr *gateway.CalledProcessError
	var spErr *pysubprocess.CalledProcessError
	return errors.As(err, &gwErr) || errors.As(err, &spErr)
}

// Stops the installed systemd, launchd or Windows service, true when one was stopped
func stopInstalledService(ctx context.Context, system bool) (bool, error) {
	kind, err := installedServiceKind(ctx)
	if err != nil || kind == "" {
		return false, err
	}
	err = gateway.ServiceCall(ctx, kind, "stop", system)
	var exit *gateway.SystemExitError
	switch {
	case err == nil:
		return true, nil
	case isCalledProcessError(err):
		return false, nil
	case kind == "windows" && !errors.As(err, &exit):
		return false, nil
	}
	return false, err
}

// Refuses a self targeting stop, restart or uninstall from inside the gateway process
func refuseFromInsideGateway(w io.Writer, verb, reason string) error {
	if !tools.IsSupervisedGatewayProcess() {
		return nil
	}
	gatewayConsole(w).PrintError(fmt.Sprintf("Refusing to %s the gateway from inside the gateway process.\n"+
		"This command was blocked to prevent %s.\n"+
		"Use `icebath gateway %s` from a shell outside the running gateway.", verb, reason, verb))
	return exitWith(1)
}

// Exits 78 when a named profile served by the multiplexer must not run its own gateway
func guardNamedProfileUnderMultiplexer(ctx context.Context, w io.Writer, force bool) error {
	refused, err := gateway.NamedProfileRefusedUnderMultiplexer(ctx, w, force)
	if err != nil || !refused {
		return err
	}
	return exitWith(gateway.GatewayFatalConfigExitCode)
}

// Installs the systemd unit honoring the start flags, prompting on a terminal and defaulting to yes headless
func installSystemdFromCLI(ctx context.Context, w io.Writer, ns map[string]any, force, system bool, runAsUser string) error {
	con := gatewayConsole(w)
	if hostinfo.IsWSL() {
		con.PrintWarning("WSL detected — systemd services may not survive WSL restarts.")
		con.PrintInfo("  Consider running in foreground instead: icebath gateway run")
		con.PrintInfo("  Or use tmux/screen for persistence: tmux new -s icebath 'icebath gateway run'")
		fmt.Fprintln(w)
	}
	nonInteractive := !con.StdinIsTTY()
	flag := func(name, question string) bool {
		if v, ok := ns[name].(bool); ok {
			return v
		}
		if nonInteractive {
			return true
		}
		return con.PromptYesNo(ctx, question, true)
	}
	startNow := flag("start_now", "Start the gateway now after installing the service?")
	startOnLogin := flag("start_on_login", "Start the gateway automatically on login/boot with systemd?")
	if err := gateway.SystemdInstall(ctx, w, force, system, runAsUser, startOnLogin, nonInteractive, nsFlag(ns, "force_unit_path")); err != nil {
		return err
	}
	if startNow {
		return gateway.SystemdStart(ctx, system)
	}
	return nil
}

func cmdSetup(ctx context.Context, w io.Writer) error { return gateway.GatewaySetup(ctx, w) }

func cmdInstall(ctx context.Context, w io.Writer, ns map[string]any) error {
	h := home.Dir(ctx)
	if config.ManagedSystem(ctx, h) != "" {
		config.ManagedError(ctx, h, "install gateway service")
		return nil
	}
	if nsFlag(ns, "if_missing") {
		installed, err := gateway.IsServiceInstalled(ctx)
		if err != nil {
			return err
		}
		if installed {
			fmt.Fprintln(w, "✓ Gateway service already installed")
			return nil
		}
	}
	force := nsFlag(ns, "force")
	if err := guardNamedProfileUnderMultiplexer(ctx, w, force); err != nil {
		return err
	}
	system := nsFlag(ns, "system")
	runAsUser := nsText(ns, "run_as_user")
	if serviceMgmtBlocked() {
		if err := noBackendExit(w, "install", "termux"); err != nil {
			return err
		}
	}
	backend := gateway.ServiceBackendName(ctx, true)
	var err error
	switch backend {
	case "systemd":
		if gateway.RefusesContainerUserScopeInstall(w, system) {
			return exitWith(1)
		}
		err = installSystemdFromCLI(ctx, w, ns, force, system, runAsUser)
	case "launchd":
		startNow, isBool := ns["start_now"].(bool)
		err = gateway.LaunchdInstall(ctx, w, force, !isBool || startNow, nsFlag(ns, "force_unit_path"))
	case "windows":
		err = gateway.WindowsInstall(ctx, gateway.WindowsInstallOptions{Force: force, StartNow: optionalBool(ns["start_now"]),
			StartOnLogin: optionalBool(ns["start_on_login"]), ElevatedHandoff: nsFlag(ns, "elevated_handoff")})
	default:
		err = handleNoBackend(w, "install", true, true)
	}
	if err != nil {
		return err
	}
	if backend != "" {
		installed, err := gateway.IsServiceInstalled(ctx)
		if err != nil {
			return err
		}
		if installed {
			return gateway.RecordServiceChoice(ctx, "install", nil)
		}
	}
	return nil
}

// Pointer form of an optional namespace boolean, nil for None
func optionalBool(v any) *bool {
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}

func cmdUninstall(ctx context.Context, w io.Writer, ns map[string]any) error {
	if err := refuseFromInsideGateway(w, "uninstall", "the gateway from terminating itself"); err != nil {
		return err
	}
	h := home.Dir(ctx)
	if config.ManagedSystem(ctx, h) != "" {
		config.ManagedError(ctx, h, "uninstall gateway service")
		return nil
	}
	system := nsFlag(ns, "system")
	if serviceMgmtBlocked() {
		if err := noBackendExit(w, "uninstall", "termux"); err != nil {
			return err
		}
	}
	if backend := gateway.ServiceBackendName(ctx, true); backend != "" {
		return gateway.ServiceCall(ctx, backend, "uninstall", system)
	}
	return handleNoBackend(w, "uninstall", false, true)
}

// Whether the host gateway runs on this process home
func hostMultiplexerIsOurs(owner *gateway.HostGateway) bool {
	same, err := gateway.SameHermesHome(owner.Home, home.Process())
	return err == nil && same
}

// Names the per profile gateways an all verb no longer sweeps, never killing them
func printUnfoldedGatewayNote(ctx context.Context, w io.Writer, owner *gateway.HostGateway) {
	pids, err := gateway.FindGatewayPIDsFor(ctx, nil, true)
	if err != nil {
		return
	}
	others := []int{}
	for _, pid := range pids {
		if pid != owner.PID {
			others = append(others, pid)
		}
	}
	if len(others) == 0 {
		return
	}
	fmt.Fprintf(w, "  %d per-profile gateway process(es) still run beside it (PIDs: %s).\n", len(others), joinInts(others, ", "))
	fmt.Fprintln(w, "  They were left running; fold them in with: icebath gateway migrate --multiplex")
}

func cmdStart(ctx context.Context, w io.Writer, ns map[string]any) error {
	handled, err := ProfileLifecycle(ctx, w, "start", ns)
	if err != nil || handled {
		return err
	}
	system, startAll, force := nsFlag(ns, "system"), nsFlag(ns, "all"), nsFlag(ns, "force")
	scoped := ctx
	if startAll {
		var owner *gateway.HostGateway
		if !force {
			owner = hostMultiplexerForAllVerb()
		}
		if owner != nil {
			fmt.Fprintf(w, "✓ The host gateway is already running — %s\n", owner.Describe())
			fmt.Fprintln(w, "  One gateway per host serves every profile; nothing to start.")
			printUnfoldedGatewayNote(ctx, w, owner)
			return nil
		}
		var restore func()
		scoped, restore = hostScopeForAllVerb(ctx, owner)
		defer restore()
	} else {
		if err := guardNamedProfileUnderMultiplexer(ctx, w, force); err != nil {
			return err
		}
		dispatched, err := dispatchViaServiceManagerIfS6(ctx, w, "start", "")
		if err != nil || dispatched {
			return err
		}
	}
	if startAll {
		killed, err := gateway.KillGatewayProcesses(scoped, w, false, nil, true)
		if err != nil {
			return err
		}
		if killed > 0 {
			fmt.Fprintf(w, "✓ Killed %d stale gateway process(es) across all profiles\n", killed)
			gateway.WaitForGatewayExitForcing(10.0, 5.0)
			if _, err := gateway.WaitForAPIServerPortFree(scoped, 10.0); err != nil {
				return err
			}
		}
	}
	if serviceMgmtBlocked() {
		if err := noBackendExit(w, "start", "termux"); err != nil {
			return err
		}
	}
	if backend := gateway.ServiceBackendName(scoped, true); backend != "" {
		return gateway.ServiceCall(scoped, backend, "start", system)
	}
	return handleNoBackend(w, "start", true, false)
}

func cmdStop(ctx context.Context, w io.Writer, ns map[string]any) error {
	if err := refuseFromInsideGateway(w, "stop", "restart loops"); err != nil {
		return err
	}
	handled, err := ProfileLifecycle(ctx, w, "stop", ns)
	if err != nil || handled {
		return err
	}
	stopAll, system := nsFlag(ns, "all"), nsFlag(ns, "system")
	if !stopAll {
		pids, err := gateway.FindGatewayPIDsFor(ctx, nil, false)
		if err != nil {
			return err
		}
		served := false
		if len(pids) == 0 {
			if gateway.ServedByAnotherHostGateway(ctx, nil) != nil {
				served = true
			} else if served, err = gateway.NamedProfileServedByRunningMultiplexerCtx(ctx, nil); err != nil {
				return err
			}
		}
		if served {
			owner := gateway.ServedByAnotherHostGateway(ctx, nil)
			name, err := gateway.CurrentProfileName(ctx)
			if err != nil {
				return err
			}
			gatewayConsole(w).PrintError(fmt.Sprintf("The host gateway serves profile '%s' — there is no separate gateway for this profile to stop.", name))
			ownerFlag := ""
			if owner != nil {
				fmt.Fprintf(w, "  %s\n", owner.Describe())
				ownerFlag = "-p " + owner.ProfileLabel() + " "
			}
			fmt.Fprintln(w, "  Stop or restart the host gateway instead:")
			fmt.Fprintln(w)
			fmt.Fprintf(w, "    icebath %sgateway stop      # takes every served profile offline\n", ownerFlag)
			fmt.Fprintf(w, "    icebath %sgateway restart\n", ownerFlag)
			return exitWith(gateway.GatewayFatalConfigExitCode)
		}
	}
	if err := RefuseWhileSessionsLive(ctx, w, "stop", ns, "", ""); err != nil {
		return err
	}
	if stopAll {
		if dispatched, err := dispatchAllViaServiceManagerIfS6(ctx, w, "stop"); err != nil || dispatched {
			return err
		}
	} else if dispatched, err := dispatchViaServiceManagerIfS6(ctx, w, "stop", ""); err != nil || dispatched {
		return err
	}
	if stopAll {
		scoped, restore := hostScopeForAllVerb(ctx, hostMultiplexerForAllVerb())
		serviceAvailable, err := stopInstalledService(scoped, system)
		restore()
		if err != nil {
			return err
		}
		total, err := gateway.KillGatewayProcesses(ctx, w, false, nil, true)
		if err != nil {
			return err
		}
		if serviceAvailable {
			total++
		}
		if total > 0 {
			fmt.Fprintf(w, "✓ Stopped %d gateway process(es) across all profiles\n", total)
		} else {
			fmt.Fprintln(w, "✗ No gateway processes found")
		}
		return nil
	}
	stopped, err := stopInstalledService(ctx, system)
	if err != nil {
		return err
	}
	if stopped {
		name, err := gateway.GetServiceName(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "✓ Stopped %s service\n", name)
		return nil
	}
	ok, err := gateway.StopProfileGateway(ctx)
	if err != nil {
		return err
	}
	if ok {
		fmt.Fprintln(w, "✓ Stopped gateway for this profile")
		return nil
	}
	if code := gateway.ReportProfileStopFailure(ctx, w); code != 0 {
		return exitWith(code)
	}
	return nil
}

// Signals the host gateway and nothing else, returning how many processes were signalled
func stopHostMultiplexer(ctx context.Context, w io.Writer, owner *gateway.HostGateway) (int, error) {
	stopped, err := gateway.KillGatewayProcesses(ctx, w, false, nil, false)
	if err != nil || stopped > 0 {
		return stopped, err
	}
	if err := gateway.TerminatePid(owner.PID, false, nil); err != nil {
		return 0, nil
	}
	return 1, nil
}

var statusRunningHints = map[string][]string{
	"termux":  {"Termux note:", "  Android may stop background jobs when Termux is suspended"},
	"wsl":     {"WSL note:", "  The gateway is running in foreground/manual mode (recommended for WSL).", "  Use tmux or screen for persistence across terminal closes."},
	"windows": {"To install as a Windows Scheduled Task (auto-start on login):", "  icebath gateway install"},
	"other":   {"To install as a service:", "  icebath gateway install", "  sudo icebath gateway install --system"},
}

var statusStoppedHints = map[string][]string{
	"termux":  {"  nohup icebath gateway run > ~/.icebath/logs/gateway.log 2>&1 &  # Best-effort background start"},
	"wsl":     {"  tmux new -s icebath 'icebath gateway run'         # persistent via tmux", "  nohup icebath gateway run > ~/.icebath/logs/gateway.log 2>&1 &  # background"},
	"windows": {"  icebath gateway install  # Install as Windows Scheduled Task (auto-start on login)"},
	"other":   {"  icebath gateway install  # Install as user service", "  sudo icebath gateway install --system  # Install as boot-time system service"},
}

func statusHostKind() string {
	switch {
	case hostinfo.IsTermux():
		return "termux"
	case hostinfo.IsWSL():
		return "wsl"
	case gateway.IsWindows():
		return "windows"
	}
	return "other"
}

func cmdStatus(ctx context.Context, w io.Writer, ns map[string]any) error {
	deep, full, system := nsFlag(ns, "deep"), nsFlag(ns, "full"), nsFlag(ns, "system")
	snapshot, err := GetGatewayRuntimeSnapshot(ctx, system)
	if err != nil {
		return err
	}
	parked, err := PrintParkedStatus(ctx, w)
	if err != nil {
		return err
	}
	if parked && !snapshot.Running() {
		return nil
	}
	active := profiles.GetActiveProfileName(ctx)
	if active == "" {
		active = "default"
	}
	activeStandalone := false
	if active != "default" {
		if activeStandalone, err = profiles.ProfileIsStandalone(ctx, home.Dir(ctx)); err != nil {
			return err
		}
	}
	if activeStandalone {
		fmt.Fprintln(w, "standalone by config (gateway.standalone: true) — temporary compatibility shim")
		fmt.Fprintf(w, "  %s\n", gateway.StandaloneDeprecationNotice)
	}
	windowsServiceInstalled := false
	if gateway.IsWindows() {
		if windowsServiceInstalled, err = gateway.WindowsIsInstalled(ctx); err != nil {
			return err
		}
	}
	satellite := false
	if !activeStandalone && !snapshot.Running() {
		if satellite, err = gateway.NamedProfileServedByRunningMultiplexerCtx(ctx, nil); err != nil {
			return err
		}
	}
	health := func() error { return printRuntimeHealth(ctx, w) }
	kind := ""
	if !satellite {
		if kind, err = gateway.InstalledServiceKindFor(ctx, func() (bool, error) { return windowsServiceInstalled, nil }); err != nil {
			return err
		}
	}
	switch {
	case satellite:
		fmt.Fprintln(w, "✓ Gateway is running via the default-profile multiplexer")
		fmt.Fprintln(w, "  Manage it from the default profile: icebath gateway status")
		printServedIngressURLs(w, active)
		printUnservedSharedIngress(w, active)
	case kind != "":
		switch kind {
		case "systemd":
			err = gateway.SystemdStatus(ctx, w, deep, system, full, health)
		case "launchd":
			err = gateway.LaunchdStatus(ctx, w, deep)
		default:
			err = gateway.WindowsStatus(ctx, w, deep)
		}
		if err != nil {
			return err
		}
		printGatewayProcessMismatch(ctx, w, snapshot)
		ReportIncompatibleOwner(ctx, w, nil)
		printMultiplexStandaloneReason(ctx, w)
		printServedIngressURLs(w, "")
	default:
		pids := snapshot.GatewayPIDs
		switch {
		case len(pids) > 0 && ReportIncompatibleOwner(ctx, w, pids):
		case len(pids) > 0:
			fmt.Fprintf(w, "✓ Gateway is running (PID: %s)\n", joinInts(pids, ", "))
			fmt.Fprintln(w, "  (Running manually, not as a system service)")
			if err := health(); err != nil {
				return err
			}
			printMultiplexStandaloneReason(ctx, w)
			printServedIngressURLs(w, "")
			fmt.Fprintln(w)
			printLines(w, statusRunningHints[statusHostKind()]...)
		case snapshot.ServiceRunning:
			fmt.Fprintf(w, "✓ Gateway is running (supervised by %s)\n", snapshot.Manager)
			if err := health(); err != nil {
				return err
			}
			printMultiplexStandaloneReason(ctx, w)
			printServedIngressURLs(w, "")
		default:
			fmt.Fprintln(w, "✗ Gateway is not running")
			if err := health(); err != nil {
				return err
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w, "To start:")
			fmt.Fprintln(w, "  icebath gateway run      # Run in foreground")
			printLines(w, statusStoppedHints[statusHostKind()]...)
		}
	}
	PrintDuplicateCredentialWarnings(ctx, w)
	printOtherProfilesGatewayStatus(ctx, w)
	return PrintStandaloneByConfig(ctx, w)
}

func cmdList(ctx context.Context, w io.Writer) error { return gatewayList(ctx, w) }

func cmdMigrateLegacy(ctx context.Context, w io.Writer, ns map[string]any) error {
	if !gateway.SupportsSystemdServices(ctx) && !gateway.IsMacOS() {
		fmt.Fprintln(w, "Legacy unit migration only applies to systemd-based Linux hosts.")
		return nil
	}
	_, _, err := gateway.RemoveLegacyHermesUnits(ctx, w, !nsFlag(ns, "yes"), nsFlag(ns, "dry_run"))
	return err
}

// Exit status of a handler error, the SystemExit code or the guidance for a systemd failure
func gatewayCommandExit(w io.Writer, err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exit *gateway.SystemExitError
	if errors.As(err, &exit) {
		return exit.Code, nil
	}
	con := gatewayConsole(w)
	var unavailable *gateway.UserSystemdUnavailableError
	if errors.As(err, &unavailable) {
		con.PrintError("User systemd not reachable:")
		gateway.PrintIndented(unavailable.Error(), func(line string) { fmt.Fprintln(w, line) })
		return 1, nil
	}
	var root *gateway.SystemScopeRequiresRootError
	if errors.As(err, &root) {
		fmt.Fprintln(w, root.Error())
		return 1, nil
	}
	if lines := ExplainServiceFailure(err); lines != nil {
		con.PrintError(lines[0])
		gateway.PrintIndented(strings.Join(lines[1:], "\n"), func(line string) { fmt.Fprintln(w, line) })
		return 1, nil
	}
	return 1, err
}
