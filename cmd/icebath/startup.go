package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/app"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/builtin/googlemeet"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Release date of this build, hermes_cli.__release_date__

// Version the build stamps through ldflags, empty for a source build
var version string

// Hidden child modes a re-executed binary dispatches before anything else, the Python child entry scripts
func dispatchChildEntries() {
	home.DispatchScratchPruneEntry()
	googlemeet.DispatchMeetBotEntry()
	botdesktop.DispatchIdleWatchEntry()
	gateway.DispatchStderrTimestampEntry()
	gateway.DispatchSystemdStopMarkEntry()
	gateway.DispatchCgroupCleanupEntry()
	tools.DispatchDDGSWorkerEntry()
	tools.DispatchCommitGraphRefreshEntry()
	provider.DispatchCodexToolsMCPServerEntry()
	cron.DispatchExternalWorkerEntry()
}

// Shows icebath instead of the binary path in ps and top, never fatal
func setProcessTitle() {
	setProcessTitleOS("icebath")
}

// Warns when this process is PID 1 with nothing above it to reap orphans
func warnIfUnsupervisedPID1(pid int) {
	if pid != 1 {
		return
	}
	fmt.Fprintln(os.Stderr, "[icebath] WARNING: this process is PID 1 with no init above it "+
		"(entrypoint override?). Orphaned child processes will not be "+
		"reaped and will accumulate as zombies. Use the image's default "+
		"ENTRYPOINT (docker/entrypoint-dispatch.sh) instead of overriding "+
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

// Cheap probe for an active non default profile redirecting the home
func activeProfileMayOverrideHome(root string) bool {
	raw, err := os.ReadFile(filepath.Join(root, "active_profile"))
	if err != nil {
		return false
	}
	active := strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff"))
	return active != "" && active != "default"
}

// Conservative probe for container mode routing, any profile ambiguity meaning it may be active
func containerModeMayBeActive() bool {
	if os.Getenv("ICEBATH_DEV") == "1" || hostinfo.IsContainer() {
		return false
	}
	if h := strings.TrimSpace(os.Getenv(home.EnvHome)); h != "" {
		if _, err := os.Stat(filepath.Join(h, ".container-mode")); err == nil {
			return true
		}
		parent := filepath.Base(filepath.Dir(filepath.Clean(h)))
		return parent != home.ProfilesDir && activeProfileMayOverrideHome(h)
	}
	def := home.PlatformDefault()
	if activeProfileMayOverrideHome(def) {
		return true
	}
	_, err := os.Stat(filepath.Join(def, ".container-mode"))
	return err == nil
}

// Answers icebath --version before config and logging, never when container mode may route it
func tryFastVersion(argv []string) bool {
	if !isGlobalFastVersionArgv(argv) || containerModeMayBeActive() {
		return false
	}
	printVersionInfo(true)
	return true
}

// Version label the startup banner title shows
func bannerVersionLabel() string { return app.FormatBannerVersionLabel(context.Background()) }

// The canonical icebath --version output, hermes_cli/_startup_fast.py print_fast_version_info
func printVersionInfo(checkUpdates bool) {
	fmt.Println(bannerVersionLabel())
	root := pm.RepoRoot()
	fmt.Println("Install directory: " + root)
	ctx := context.Background()
	method, err := config.DetectInstallMethod(home.Dir(ctx), root)
	if err != nil {
		method = readInstallMethodStamp()
	}
	if method != "" {
		fmt.Println("Install method: " + method)
	}
	fmt.Println("Go: " + strings.TrimPrefix(runtime.Version(), "go"))
	if !checkUpdates {
		return
	}
	status, err := selfupdate.CheckForUpdates(ctx, selfupdate.SourceCheckOptions{InstallRoot: root, Home: home.Dir(ctx), Passive: true})
	if err != nil || status == nil {
		return
	}
	command, _ := config.RecommendedUpdateCommand(home.Dir(ctx))
	behind, _ := status.Get("behind")
	rollback, _ := status.Get("rollback")
	sourceVersion, _ := status.Get("sourceVersion")
	switch {
	case truthy(rollback):
		fmt.Printf("Newer than stable release v%v — '%s' moves back to it\n", sourceVersion, command)
	case intOf(behind) == selfupdate.UpdateAvailableNoCount && behind != nil:
		fmt.Printf("Update available — run '%s'\n", command)
	case behind != nil && intOf(behind) > 0:
		word := "commits"
		if intOf(behind) == 1 {
			word = "commit"
		}
		fmt.Printf("Update available: %d %s behind — run '%s'\n", intOf(behind), word, command)
	case behind != nil && intOf(behind) == 0:
		fmt.Println("Up to date")
	}
}

// The installer's .install_method stamp in the resolved home, lower cased
func readInstallMethodStamp() string {
	h := strings.TrimSpace(os.Getenv(home.EnvHome))
	if h == "" {
		h = home.PlatformDefault()
	}
	raw, err := os.ReadFile(filepath.Join(h, ".install_method"))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff")))
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

// One passive PM verdict on every dispatch, the problem text or empty
func checkRuntime(installRoot string) string {
	return selfupdate.CheckRuntime(context.Background(), installRoot)
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}
