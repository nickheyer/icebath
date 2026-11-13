package main

import (
	"context"
	"fmt"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/acp"
	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/containerinit/stage"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/botmode/dmdelivery"
	"github.com/nickheyer/icebath/pkg/builtin/googlemeet"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/relayhost"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/startupfast"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Release date of this build, hermes_cli.__release_date__

// Version the build stamps through ldflags, empty for a source build
var version string

// Assigns every process hook and then runs the hidden child entry argv names, the first step of main
func startProcess(ctx context.Context) {
	updaterecord.SetBuildVersion(version)
	logging.InstallSlogBridge()
	installProcessWiring()
	dispatchChildEntries(ctx)
}

// Hidden child modes a re-executed binary dispatches once the hooks their Python imports install are wired
func dispatchChildEntries(ctx context.Context) {
	selfupdate.DispatchCustodyLauncher()
	selfupdate.DispatchDesktopUpdateVerifyEntry(ctx, exit)
	selfupdate.DispatchWindowsAppInstallerUpdateEntry(exit)
	home.DispatchScratchPruneEntry(ctx)
	relayhost.DispatchRelayHostEntry(ctx)
	googlemeet.DispatchMeetBotEntry(ctx)
	botdesktop.DispatchIdleWatchEntry()
	dmdelivery.DispatchEntry(ctx, exit)
	gateway.DispatchStderrTimestampEntry()
	gateway.DispatchGatewayRestartWatcherEntry(ctx)
	gateway.DispatchDetachedRestartWatcherEntry(ctx)
	gateway.DispatchUpdateHelperEntry(ctx)
	gateway.DispatchSystemdStopMarkEntry(ctx)
	stage.DispatchStageEntry(ctx)
	gateway.DispatchCgroupCleanupEntry()
	gateway.DispatchKanbanWorkerClientEntry(ctx)
	gateway.DispatchWindowsSSHRuntimeEntry(ctx, exit)
	tools.DispatchDDGSWorkerEntry()
	tools.DispatchCommitGraphRefreshEntry(ctx)
	provider.DispatchCodexToolsMCPServerEntry()
	cron.DispatchExternalWorkerEntry(exit)
	agent.DispatchManagedWorkerEntry(exit)
	app.DispatchUninstallModuleEntry(ctx)
	app.DispatchWindowsStoreUpdateEntry()
	app.DispatchSlashWorkerEntry(ctx, exit)
	app.DispatchCLIModuleEntry(ctx, exit)
	tuiserver.DispatchComputeHostEntry(ctx, exit)
	tuiserver.DispatchTUIGatewayEntry(ctx, exit)
	gatewayrun.DispatchGatewayRunModuleEntry(ctx, exit)
	acp.DispatchModuleEntry(ctx, exit)
}

// Shows icebath instead of the binary path in ps and top, never fatal
func setProcessTitle() {
	setProcessTitleOS("icebath")
	releaseTitleThread()
}

// Warns when this process is PID 1 with nothing above it to reap orphans
func warnIfUnsupervisedPID1(pid int) {
	if pid != 1 {
		return
	}
	fmt.Fprintln(os.Stderr, "[icebath] WARNING: this process is PID 1 with no init above it "+
		"(entrypoint override?). Orphaned child processes will not be "+
		"reaped and will accumulate as zombies. Use the image's default "+
		"ENTRYPOINT (/init) instead of overriding "+
		"it, or run with `docker run --init` / `init: true` in Compose.")
}

// Advertises the agent harness to child processes without clobbering an outer harness
func advertiseAgentEnv() {
	if _, ok := os.LookupEnv("AI_AGENT"); !ok {
		os.Setenv("AI_AGENT", "icebath-agent")
	}
	if _, ok := os.LookupEnv("ICEBATH_AGENT"); !ok {
		os.Setenv("ICEBATH_AGENT", "true")
	}
}

// One TLS authority, the OS store trusted before any outbound call resolves a CA bundle
func installTruststore() {
	provider.InstallTrustStore()
}

// Cheap display.interface read for the earliest hot path decisions, cached per config path
var earlyInterfaceCache struct {
	sync.Mutex
	path, value string
}

func earlyInterfaceConfigPath() string {
	if h := os.Getenv(home.EnvHome); h != "" {
		return filepath.Join(h, "config.yaml")
	}
	return filepath.Join(home.PlatformDefault(), "config.yaml")
}

// Configured default interface, cli or tui, any error reading as cli
func configDefaultInterfaceEarly() string {
	cfgPath := earlyInterfaceConfigPath()
	earlyInterfaceCache.Lock()
	defer earlyInterfaceCache.Unlock()
	if earlyInterfaceCache.path == cfgPath && earlyInterfaceCache.value != "" {
		return earlyInterfaceCache.value
	}
	value := "cli"
	ctx := context.Background()
	if exists, err := config.ConfigExists(ctx, cfgPath); err == nil && exists {
		if raw, err := config.ReadConfigDocReadonly(ctx, cfgPath); err == nil {
			if doc, ok := raw.(map[string]any); ok {
				if disp, ok := doc["display"].(map[string]any); ok {
					if iface, ok := disp["interface"].(string); ok && strings.ToLower(strings.TrimSpace(iface)) == "tui" {
						value = "tui"
					}
				}
			}
		}
	}
	earlyInterfaceCache.path, earlyInterfaceCache.value = cfgPath, value
	return value
}

// Earliest TUI decision, --cli wins, then --tui or ICEBATH_TUI, then a real TTY gate, then display.interface
func wantsTUIEarly(argv []string) bool {
	for _, a := range argv {
		if a == "--cli" {
			return false
		}
	}
	if os.Getenv("ICEBATH_TUI") == "1" {
		return true
	}
	for _, a := range argv {
		if a == "--tui" || a == "--native" || a == "--tui-native" {
			return true
		}
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
		return false
	}
	return configDefaultInterfaceEarly() == "tui"
}

// Turns every known mouse tracking mode off before the TUI takes stdin raw
func suppressMouseResidueEarly(argv []string) {
	if os.Getenv("ICEBATH_TUI_NO_EARLY_DISABLE") == "1" || !wantsTUIEarly(argv) {
		return
	}
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		return
	}
	os.Stdout.WriteString("\x1b[?1003l\x1b[?1002l\x1b[?1001l\x1b[?1000l\x1b[?9l\x1b[?1006l\x1b[?1005l\x1b[?1015l\x1b[?1016l\x1b[?2029l")
}

// Only --version or -V alone
func isGlobalFastVersionArgv(argv []string) bool {
	return len(argv) == 1 && (argv[0] == "--version" || argv[0] == "-V")
}

// Answers icebath --version before config and logging, never when container mode may route it
func tryFastVersion(argv []string) bool {
	if !isGlobalFastVersionArgv(argv) || startupfast.ContainerModeMayBeActive() {
		return false
	}
	printVersionInfo(true)
	return true
}

// The canonical icebath --version output on stdout, the update status line when asked
func printVersionInfo(checkUpdates bool) {
	startupfast.PrintFastVersionInfo(context.Background(), os.Stdout, checkUpdates)
}

// Loads the home .env over shell exports with the project root .env as the dev fallback
func loadDotenv() {
	ctx := context.Background()
	projectEnv := filepath.Join(pm.RepoRoot(), ".env")
	if _, err := provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{ProjectEnv: projectEnv}); err != nil {
		slog.Debug(fmt.Sprintf("dotenv load failed: %v", err))
	}
}

// Bridges security.redact_secrets into the env before logging snapshots it, reporting network.force_ipv4
func applyEarlyConfig() bool {
	ctx := context.Background()
	cfgPath := home.ConfigPath(home.Dir(ctx))
	exists, err := config.ConfigExists(ctx, cfgPath)
	if err != nil || !exists {
		return false
	}
	raw, err := config.LoadUserEffective(cfgPath, nil)
	if err != nil {
		return false
	}
	if _, set := os.LookupEnv("ICEBATH_REDACT_SECRETS"); !set {
		if sec, ok := raw["security"].(map[string]any); ok {
			if redact, ok := sec["redact_secrets"]; ok && redact != nil {
				os.Setenv("ICEBATH_REDACT_SECRETS", strings.ToLower(pyStr(redact)))
			}
		}
	}
	if net, ok := raw["network"].(map[string]any); ok && truthy(net["force_ipv4"]) {
		return true
	}
	return false
}

// Long lived server launches cap glibc malloc arenas before any thread exists
func serverProcess(argv []string) bool {
	ok, err := gateway.ServerProcessArgv(argv)
	return err == nil && ok
}

func applyMallocArenaMax() {
	gateway.ApplyMallocArenaMax(nil)
}

// Centralized file logging for every subcommand, gui mode for the dashboard entry points
func setupLogging(argv []string) {
	mode := "cli"
	for _, a := range argv {
		if strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "dashboard", "serve", "gui", "desktop":
			mode = "gui"
		}
		break
	}
	if _, err := logging.SetupLogging(context.Background(), logging.Options{Mode: mode}); err != nil {
		slog.Debug(fmt.Sprintf("logging setup failed: %v", err))
	}
}

// Termux check safe for cold start sensitive paths
func isTermuxStartupEnvironment() bool { return selfupdate.IsTermuxEnv(nil) }

// Container mode metadata when the NixOS activation routes every invocation into the managed container
func containerExecInfo() *config.ContainerExecInfo {
	info, err := config.GetContainerExecInfo(home.Dir(context.Background()))
	if err != nil {
		return nil
	}
	return info
}

// Replaces this process with the same command inside the managed container, hermes_cli/main_container.py
func execInContainer(info *config.ContainerExecInfo, cliArgs []string) {
	code, err := config.ExecInContainer(info, cliArgs)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	exit(code)
}

// Arms the gateway startup liveness watchdog before the heavy startup
func armStartupWatchdog() {
	gateway.ArmStartupWatchdog(context.Background())
}

// Puts the old tree back after an update killed mid swap, true when a restore happened
func restoreInterruptedPull() bool {
	return selfupdate.RestoreInterruptedPull(context.Background(), pm.RepoRoot(), false)
}

// Re executes the command once the restore landed
func relaunchAfterRestore() {
	exit(selfupdate.RelaunchAfterRestore(os.Args))
}

// Re stages the Windows bin launchers from the default root, inaction everywhere else
func ensureBinLaunchers() {
	if runtime.GOOS != "windows" {
		return
	}
	selfupdate.EnsureWindowsBinLaunchers(pm.RepoRoot(), nil, nil)
}

// Sweeps stale quarantined Windows executables from earlier updates
func cleanupQuarantinedExes() {
	if runtime.GOOS != "windows" {
		return
	}
	selfupdate.CleanupQuarantinedExes(context.Background(), pm.RepoRoot(), "")
}

// Reports a fleet restart a previous update still owes and recovers gateways a killed update paused
func recoverUpdateDebtsOnStartup() {
	ctx := context.Background()
	if !containsArg(os.Args[1:], "update") {
		selfupdate.WarnPendingFleetRestartOnStartup(ctx, home.Dir(ctx), os.Stderr)
	}
	selfupdate.RecoverPausedGateways(ctx, os.Args[1:])
}

// Runs the boot bootstrap when the install identity changed since the last recorded run
func maybeRunBootBootstrap(projectRoot string) {
	selfupdate.MaybeRunBootBootstrap(context.Background(), projectRoot)
}

// Prints the install's runtime command for a launch asked for it and exits with its status
func printLauncherRuntimeCommand(pinned bool) {
	if pinned || len(os.Args) < 2 || os.Args[1] != "--print-runtime-command" {
		return
	}
	exit(pm.LaunchersMain(context.Background(), pm.RepoRoot(), os.Args[1:], os.Stdout, os.Stderr))
}

// Finishes an owed source update and activates committed dependencies before the command is parsed
func bootLaunch() {
	os.Args = selfupdate.TakeRelaunchMarker(os.Args)
	pm.SetCommandArgv(commands.NewCLI().CommandArgv)
	if code, stop := selfupdate.BootLaunch(context.Background(), pm.RepoRoot(), os.Args); stop {
		exit(code)
	}
}

// One passive PM verdict on every dispatch, the problem text or empty
func checkRuntime(installRoot string) string {
	problem, err := selfupdate.CheckRuntime(context.Background(), installRoot)
	if err != nil {
		slog.Debug("pm startup check failed", "error", err)
		return ""
	}
	return problem
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}
