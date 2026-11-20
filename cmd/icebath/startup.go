package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/botmode/dmdelivery"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/plugins/defaults"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/startupfast"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Release date of this build

// Version the build stamps through ldflags, empty for a source build
var version string

// Assigns every process hook and then runs the hidden child entry argv names, the first step of main
func startProcess(ctx context.Context) {
	updaterecord.SetBuildVersion(version)
	pm.SetBuildTag(version)
	logging.InstallSlogBridge()
	installProcessWiring()
	dispatchChildEntries(ctx)
}

// Hidden child modes a re-executed binary dispatches once the hooks their Python imports install are wired
func dispatchChildEntries(ctx context.Context) {
	runChildEntries(ctx, childEntriesFirst)
	home.DispatchScratchPruneEntry(ctx)
	runChildEntries(ctx, childEntriesAfterPrunePlugins)
	defaults.DispatchEntry(ctx)
	runChildEntries(ctx, childEntriesAfterPruneExtras)
	dmdelivery.DispatchEntry(ctx, exit)
	gateway.DispatchStderrTimestampEntry()
	gateway.DispatchGatewayRestartWatcherEntry(ctx)
	gateway.DispatchDetachedRestartWatcherEntry(ctx)
	gateway.DispatchUpdateHelperEntry(ctx)
	gateway.DispatchSystemdStopMarkEntry(ctx)
	runChildEntries(ctx, childEntriesAfterStopMark)
	gateway.DispatchCgroupCleanupEntry()
	runChildEntries(ctx, childEntriesAfterCgroup)
	tools.DispatchDDGSWorkerEntry()
	tools.DispatchCommitGraphRefreshEntry(ctx)
	provider.DispatchCodexToolsMCPServerEntry()
	runChildEntries(ctx, childEntriesAfterCodex)
	agent.DispatchManagedWorkerEntry(exit)
	tuiserver.DispatchTUIGatewayEntry(ctx, exit)
	gatewayrun.DispatchGatewayRunModuleEntry(ctx, exit)
	runChildEntries(ctx, childEntriesLast)
}

// Dispatches the tier child entries of one slot in link order
func runChildEntries(ctx context.Context, entries []func(ctx context.Context)) {
	for _, entry := range entries {
		entry(ctx)
	}
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

// Light parse of a chat argv that opens the TUI, false for every headless or non chat launch
func earlyTUIArgs(argv []string) (*chatArgs, bool) {
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
		return nil, false
	}
	a, ok := lightChatArgs(argv)
	if !ok || a.Version || !isTUIChatLaunch(a) {
		return nil, false
	}
	return a, true
}

// Earliest TUI decision, the same rule the chat command applies after the full parse
func wantsTUIEarly(argv []string) bool {
	_, ok := earlyTUIArgs(argv)
	return ok
}

// Turns every known mouse tracking mode off before the TUI takes stdin raw
func suppressMouseResidueEarly(argv []string) {
	if os.Getenv("ICEBATH_TUI_NO_EARLY_DISABLE") == "1" || !wantsTUIEarly(argv) {
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
		pkgLog().Debug(fmt.Sprintf("dotenv load failed: %v", err))
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

// Centralized file logging for every subcommand
func setupLogging() {
	if _, err := logging.SetupLogging(context.Background(), logging.Options{Mode: "cli"}); err != nil {
		pkgLog().Debug(fmt.Sprintf("logging setup failed: %v", err))
	}
}

// Termux check safe for cold start sensitive paths
func isTermuxStartupEnvironment() bool {
	prefix := os.Getenv("PREFIX")
	return os.Getenv("TERMUX_VERSION") != "" || strings.Contains(prefix, "com.termux/files/usr") || strings.HasPrefix(prefix, "/data/data/com.termux/")
}

// Container mode metadata when the NixOS activation routes every invocation into the managed container
func containerExecInfo() *config.ContainerExecInfo {
	info, err := config.GetContainerExecInfo(home.Dir(context.Background()))
	if err != nil {
		return nil
	}
	return info
}

// Replaces this process with the same command inside the managed container
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

// Prints the install's runtime command for a launch asked for it and exits with its status
func printLauncherRuntimeCommand(pinned bool) {
	if pinned || len(os.Args) < 2 || os.Args[1] != "--print-runtime-command" {
		return
	}
	exit(pm.LaunchersMain(context.Background(), pm.RepoRoot(), os.Args[1:], os.Stdout, os.Stderr))
}

// Registers the CLI parser with pm between the steps a tier runs around it before the command is parsed
func bootLaunch() {
	runStartupPhase(phaseBeforeParser, os.Args)
	pm.SetCommandArgv(commands.NewCLI().CommandArgv)
	runStartupPhase(phaseAfterParser, os.Args)
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}
