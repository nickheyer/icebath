package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Steps of packages above this one that the module entry runs, each assigned in cmd/icebath wiring
var (
	CheckRuntime          func(ctx context.Context, projectRoot string) (string, error)
	MaybeRunBootBootstrap func(ctx context.Context, projectRoot string)
	ConfigureWindowsStdio func() bool
)

// Exit status of a Windows process an unhandled interrupt ended, STATUS_CONTROL_C_EXIT as a signed word
const controlCExitStatus = -1073741510

// Sets an env variable only when the process does not carry it yet
func setenvDefault(name, value string) {
	if _, set := os.LookupEnv(name); !set {
		os.Setenv(name, value)
	}
}

// Exit status of a run that ended in an error, the code of a SystemExit and a printed traceback line otherwise
func runMainFailureCode(err error, stderr io.Writer) int {
	var exit *gateway.SystemExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	if errors.Is(err, ErrKeyboardInterrupt) {
		fmt.Fprint(stderr, "Traceback (most recent call last):\nKeyboardInterrupt\n")
		if runtime.GOOS == "windows" {
			return controlCExitStatus
		}
		return 128 + int(syscall.SIGINT)
	}
	fmt.Fprintf(stderr, "Traceback (most recent call last):\n%s: %s\n", pyrepr.ExceptionName(err), pyrepr.ExceptionMessage(err))
	return 1
}

// Exit status of a gateway run failure, printing the traceback lines an uncaught Python exception prints
func MainFailureCode(err error, stderr io.Writer) int { return runMainFailureCode(err, stderr) }

// Runs the gateway module entry and returns its exit status, hard exiting after a started gateway tore down
func Main(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if err := ImportRunModule(ctx); err != nil {
		return runMainFailureCode(err, stderr)
	}
	if err := guardCorruptUserConfig(ctx); err != nil {
		return runMainFailureCode(err, stderr)
	}
	setenvDefault("AI_AGENT", "icebath-agent")
	setenvDefault("ICEBATH_AGENT", "true")
	bestEffort(func() (bool, error) { return gateway.ApplyMallocArenaMax(nil), nil }, "")
	bestEffort(func() (bool, error) {
		tools.RegisterSelf(ctx, "gateway", "", nil)
		return tools.AttachSelfToKillOnCloseJob(), nil
	}, "")
	bestEffort(func() (bool, error) { gateway.ArmStartupWatchdog(ctx); return true, nil }, "")
	bestEffort(func() (bool, error) { return ConfigureWindowsStdio(), nil }, "")
	if problem, ok := bestEffort(func() (string, error) { return CheckRuntime(ctx, pm.InstallRoot()) }, ""); !ok {
		runLog().Debug("pm startup check failed")
	} else if problem != "" {
		runLog().Warn(problem)
	}
	if _, ok := bestEffort(func() (bool, error) { MaybeRunBootBootstrap(ctx, pm.InstallRoot()); return true, nil }, ""); !ok {
		runLog().Debug("boot bootstrap failed")
	}
	configPath, code, done := parseRunMainArgs(argv, stdout, stderr)
	if done {
		return code
	}
	var cfg *gateway.GatewayConfig
	if configPath != nil && *configPath != "" {
		loaded, err := gateway.LoadGatewayConfigFile(ctx, *configPath)
		if err != nil {
			return runMainFailureCode(err, stderr)
		}
		cfg = loaded
		decision, err := ResolveMultiplexMode(ctx, cfg)
		if err != nil {
			return runMainFailureCode(err, stderr)
		}
		gateway.LogMultiplexDecision(ctx, decision)
	}
	exitCode := 0
	success, err := RunGateway(ctx, StartGatewayOptions{Config: cfg, Verbosity: new(int)})
	var exit *gateway.SystemExitError
	switch {
	case errors.As(err, &exit):
		exitCode = exit.Code
	case err != nil:
		return runMainFailureCode(err, stderr)
	case !success:
		exitCode = 1
	}
	exitAfterGracefulShutdown(ctx, exitCode)
	return exitCode
}

// Runs the gateway module entry and exits when argv carries its entry word
func DispatchGatewayRunModuleEntry(ctx context.Context, exit func(int)) {
	if len(os.Args) < 2 || os.Args[1] != gateway.GatewayRunModuleEntry {
		return
	}
	exit(Main(ctx, os.Args[2:], os.Stdout, os.Stderr))
}
