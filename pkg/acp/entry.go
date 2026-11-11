package acp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var entryLog = logging.GetLogger("acp_adapter.entry")

// Entry word the binary runs the ACP adapter module under
const ACPAdapterModuleEntry = "__icebath_acp_adapter__"

// Liveness probe methods outside the ACP schema whose method not found log is noise
var benignProbeMethods = map[string]bool{"ping": true, "health": true, "healthcheck": true}

// Drops the background task failure logged for an unknown liveness probe method, every other record kept
type benignProbeMethodFilter struct{}

func (benignProbeMethodFilter) Filter(rec *logging.Record) bool {
	message, _, _ := strings.Cut(rec.Msg, "\n")
	if message != "Background task failed" || rec.ExcInfo == nil {
		return true
	}
	exc, ok := rec.ExcInfo.(*pyacp.RequestError)
	if !ok || exc.Code != -32601 {
		return true
	}
	data, ok := exc.Data.(*pyjson.Object)
	if !ok {
		return true
	}
	method, _ := data.GetOr("method", nil).(string)
	return !benignProbeMethods[method]
}

// Routes all logging to stderr through the redactor so stdout stays clean for ACP stdio
var setupLogging = func() error {
	handler, err := logging.NewRedactingStderrHandler("%(asctime)s [%(levelname)s] %(name)s: %(message)s", "%Y-%m-%d %H:%M:%S")
	if err != nil {
		return err
	}
	handler.AddFilter(benignProbeMethodFilter{})
	root := logging.GetLogger("root")
	for _, existing := range root.Handlers() {
		root.RemoveHandler(existing)
	}
	root.AddHandler(handler)
	root.SetLevel(logging.LevelInfo)
	for _, noisy := range []string{"httpx", "httpcore", "openai"} {
		logging.GetLogger(noisy).SetLevel(logging.LevelWarning)
	}
	return nil
}

// Loads the .env of the profile home
var loadEnv = func(ctx context.Context) error {
	profileHome := home.Dir(ctx)
	loaded, err := provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: profileHome})
	if err != nil {
		return err
	}
	for _, envFile := range loaded {
		entryLog.Info("Loaded env from " + envFile)
	}
	if len(loaded) == 0 {
		entryLog.Info(fmt.Sprintf("No .env found at %s, using system env", filepath.Join(profileHome, ".env")))
	}
	return nil
}

func printVersion(stdout io.Writer) int {
	info, err := getVersionInfo()
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(stdout, info.DerivedVersion)
	return 0
}

func runCheck(stdout io.Writer) int {
	fmt.Fprintln(stdout, "Icebath ACP check OK")
	return 0
}

// Whether stdin is a terminal
var stdinIsTTY = func() bool { return xterm.IsTerminal(os.Stdin.Fd()) }

// Reads one line of input after printing the prompt, an error at end of input
var readInput = func(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Ensures a PM package for an explicit request
var pmEnsure = func(ctx context.Context, name string, explicit bool) error {
	_, err := pm.Ensure(ctx, name, pm.EnsureOptions{Explicit: explicit})
	return err
}

// Runs the interactive model setup, then offers the browser tools install on a terminal
func runSetup(ctx context.Context, stderr io.Writer) int {
	if code := RunModelSetup(ctx); code != 0 {
		return code
	}
	if !stdinIsTTY() {
		return 0
	}
	reply, err := readInput("\nInstall browser tools? Downloads the pinned Chromium through PM. [y/N] ")
	if err != nil {
		return 0
	}
	if reply = strings.ToLower(strings.TrimSpace(reply)); reply == "y" || reply == "yes" {
		runSetupBrowser(ctx, false, stderr)
	}
	return 0
}

// Installs PM's pinned Chromium for an explicit request, one on failure
var runSetupBrowser = func(ctx context.Context, assumeYes bool, stderr io.Writer) int {
	if err := pmEnsure(ctx, "chromium", true); err != nil {
		var installErr *pm.InstallError
		var pathErr *fs.PathError
		var linkErr *os.LinkError
		var sysErr *os.SyscallError
		var errno syscall.Errno
		if errors.As(err, &installErr) || errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr) || errors.As(err, &errno) {
			fmt.Fprintf(stderr, "Browser setup failed: %s\n", err)
			return 1
		}
		panic(err)
	}
	return 0
}

// Imports the configured memory provider module before the ACP threads start
func warmMemoryProviderImport(ctx context.Context) {
	env := &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
	if !memory.ImportMemoryProviderModule(ctx, env, "") {
		entryLog.Debug("memory provider not warmed (none configured or import failed; agent init reports that)")
	}
}

// Agent the entry point serves and closes
type entryAgent interface{ Close() }

// Agent of the entry point, the gateway projection
var newEntryAgent = func() entryAgent { return NewAgent(nil).(entryAgent) }

// Serves the agent over stdio until the editor disconnects
var runAgent = func(agent any, useUnstableProtocol bool) error {
	return pyacp.RunAgent(agent, nil, nil, useUnstableProtocol, pyacp.DefaultStdioBufferLimitBytes)
}

// Trusts the operating system certificate store before any outbound call
var installTruststore = provider.InstallTrustStore

// Entry point of the ACP adapter, its exit status
func Main(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	args, code, done := parseEntryArgs(argv, stdout, stderr)
	if done {
		return code
	}
	switch {
	case args.Version:
		return printVersion(stdout)
	case args.Check:
		return runCheck(stdout)
	case args.Setup:
		return runSetup(ctx, stderr)
	}
	if args.SetupBrowser {
		return runSetupBrowser(ctx, args.AssumeYes, stderr)
	}
	if err := setupLogging(); err != nil {
		panic(err)
	}
	if err := loadEnv(ctx); err != nil {
		panic(err)
	}
	entryLog.Info("Starting icebath-agent ACP adapter")
	installTruststore()
	if runtime.GOOS == "windows" {
		warmMemoryProviderImport(ctx)
	}
	agent := newEntryAgent()
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	defer signal.Stop(interrupted)
	served := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				served <- fmt.Errorf("%v", r)
			}
		}()
		served <- runAgent(agent, true)
	}()
	select {
	case err := <-served:
		agent.Close()
		if err != nil {
			entryLog.Exception("ACP agent crashed", err)
			return 1
		}
	case <-interrupted:
		agent.Close()
		entryLog.Info("Shutting down (KeyboardInterrupt)")
	}
	return 0
}

// Runs the adapter module entry and exits when argv carries its entry word
func DispatchModuleEntry(ctx context.Context, exit func(int)) {
	if len(os.Args) < 2 || os.Args[1] != ACPAdapterModuleEntry {
		return
	}
	exit(Main(ctx, os.Args[2:], os.Stdout, os.Stderr))
}
