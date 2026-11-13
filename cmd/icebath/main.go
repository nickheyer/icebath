package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/containerinit"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/spf13/cobra"
)

// Mirrors the import time sequence of hermes_cli/main.py and then its main()
func main() {
	if code, ok := containerinit.Dispatch(os.Args); ok {
		os.Exit(code)
	}
	if code, ok := runNixModuleVerb(os.Args[1:], os.Stdout, os.Stderr); ok {
		os.Exit(code)
	}
	containerinit.DropRootInImage(os.Args)
	startProcess(context.Background())
	args, pinned := pm.LauncherStart(pm.RepoRoot(), os.Args)
	if restoreInterruptedPull() {
		relaunchAfterRestore()
	}
	os.Args = args
	pm.PinLauncherHome()
	if handoff, tail, ok := pm.LegacyPostSwapInvocation(os.Args[1:]); ok {
		exit(selfupdate.LegacyPostSwapMain(context.Background(), handoff, tail, os.Stderr))
	}
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
	home.ExportScratchTmpEnv(context.Background())
	if len(argv) > 0 && argv[0] == "pm" {
		exit(runCLI(argv))
	}
	ensureBinLaunchers()
	loadDotenv()
	forceIPv4 := applyEarlyConfig()
	if serverProcess(argv) {
		applyMallocArenaMax()
	}
	setupLogging(argv)
	if forceIPv4 {
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
	cleanupQuarantinedExes()
	selfupdate.SweepStaleBytecodeIfCheckoutChanged()
	recoverUpdateDebtsOnStartup()
	if first, _ := firstPositional(argv); first != "update" {
		maybeRunBootBootstrap(pm.InstallRoot())
	}
	if problem := checkRuntime(pm.InstallRoot()); problem != "" {
		fmt.Fprintln(os.Stderr, "⚠ "+problem)
	}
	for _, fast := range []func([]string) (bool, int){tryTermuxFastTUILaunch, tryTermuxFastCLILaunch, tryFastServeLaunch, tryFastChatLaunch} {
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
	prepareAgentStartup(a)
	if a.Oneshot != "" {
		return &commands.ExitError{Code: runOneshotFromArgs(a)}
	}
	if a.Command == "" {
		observability.SetProcessSurface(nil)
	} else {
		observability.SetProcessSurface(a.Command)
	}
	return nil
}

// Runs the registered exit hooks and ends the process with code
func exit(code int) {
	runAtExit()
	os.Exit(code)
}
