package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/settings"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	"github.com/spf13/cobra"
)

// Mirrors the import time sequence of and then its main
func main() {
	defer refuseWithoutDataHome()
	if code, ok := runTierArgvEntries(os.Args); ok {
		os.Exit(code)
	}
	if code, ok := runNixModuleVerb(os.Args[1:], os.Stdout, os.Stderr); ok {
		os.Exit(code)
	}
	if code, ok := runSQLiteRuntimeVerb(os.Args[1:], os.Stdout, os.Stderr); ok {
		os.Exit(code)
	}
	runStartupPhase(phaseBeforeStart, os.Args)
	startProcess(context.Background())
	args, pinned := pm.LauncherStart(pm.RepoRoot(), os.Args)
	runStartupPhase(phaseAfterLauncher, os.Args)
	os.Args = args
	pm.PinLauncherHome()
	runStartupPhase(phaseAfterPinHome, os.Args)
	if legacy, ok := app.LegacyAgentArgv(os.Args); ok {
		exit(app.LegacyAgentMain(context.Background(), legacy, os.Stdout, os.Stderr, nil))
	}
	printLauncherRuntimeCommand(pinned)
	normalizeHomeEnv()
	bootLaunch()
	argv := os.Args[1:]
	if argvIsGatewayRun(argv) {
		armStartupWatchdog()
	}
	suppressMouseResidueEarly(argv)
	pm.EnsureProjectRootOnPath()
	if tryFastVersion(argv) {
		exit(0)
	}
	argv = applyProfileOverride(argv)
	if first, _ := firstPositional(argv); first != "uninstall" {
		home.ExportScratchTmpEnv(context.Background())
	}
	if len(argv) > 0 && argv[0] == "pm" {
		exit(runCLI(argv))
	}
	runStartupPhase(phaseBeforeDotenv, os.Args)
	loadDotenv()
	if serverProcess(argv) {
		applyMallocArenaMax()
	}
	setupLogging()
	if settings.NetworkForceIpv4.Get(context.Background()) {
		home.ApplyIPv4Preference(true)
	}
	exit(run(argv))
}

// The body of main.py main(), the exit status of the dispatched command
func run(argv []string) int {
	setProcessTitle()
	warnIfUnsupervisedPID1(os.Getpid())
	advertiseAgentEnv()
	installTruststore()
	runStartupPhase(phaseRun, argv)
	for _, fast := range []func([]string) (bool, int){tryTermuxFastTUILaunch, tryTermuxFastCLILaunch, tryFastChatLaunch} {
		if handled, code := fast(argv); handled {
			return code
		}
	}
	if info := containerExecInfo(); info != nil {
		execInContainer(info, os.Args[1:])
		return 1
	}
	return runCLI(argv)
}

// Parses argv with the cobra tree and runs the bound handler, the pre run doing main()'s post parse steps
func runCLI(argv []string) int {
	cli := newCLI()
	cli.Root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		return beforeDispatch(cli, cmd, args)
	}
	code, msg := commands.ExitCode(cli.Execute(argv))
	if msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	return code
}

// Version flag, yolo chokepoint, agent startup discovery, oneshot and the process surface before a handler runs
func beforeDispatch(cli *commands.CLI, cmd *cobra.Command, args []string) error {
	if v, _ := cli.Root.Flags().GetBool("version"); v {
		printVersionInfo(true)
		return &commands.ExitError{Code: 0}
	}
	a := parsedArgs(cli, cmd, args)
	if a.Yolo {
		os.Setenv("ICEBATH_YOLO_MODE", "1")
	}
	if code := adoptStdinQuery(a); code != 0 {
		return &commands.ExitError{Code: code}
	}
	prepareAgentStartup(a)
	if a.Oneshot != "" {
		return &commands.ExitError{Code: runOneshotFromArgs(a)}
	}
	return nil
}

// Ends the process with the reason, not a stack trace, when it panicked because neither ICEBATH_HOME nor a user home names a folder for the data
func refuseWithoutDataHome() {
	r := recover()
	if r == nil {
		return
	}
	if err, ok := r.(error); ok && errors.Is(err, pathutil.ErrNoHome) {
		cliui.RestoreTerminal()
		fmt.Fprintln(os.Stderr, "icebath: "+err.Error())
		os.Exit(1)
	}
	panic(r)
}

// Runs the registered exit hooks and ends the process with code
func exit(code int) {
	runAtExit()
	cliui.RestoreTerminal()
	os.Exit(code)
}
