package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// CLI constructor and runtime preparation of the worker child, the real ones in production
var (
	slashWorkerNewCLI  = NewCLI
	slashWorkerPrepare = prepareSlashWorkerRuntime
)

// Set while a slash command executes, the legacy watchdog's grace
var slashWorkerInFlight atomic.Bool

func slashWorkerLogger() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.slash_worker")
}

// Runs the slash worker child when argv names its entry
func DispatchSlashWorkerEntry(ctx context.Context, exit func(code int)) {
	if len(os.Args) < 2 || os.Args[1] != tuigateway.SlashWorkerEntry {
		return
	}
	exit(slashWorkerMain(ctx, os.Args[2:]))
}

// Exits once a pre update gateway that passed its pid dies, an in flight command getting a short grace
func startLegacyParentWatchdog(ctx context.Context, parentPID int, poll, grace time.Duration) {
	if parentPID == 0 || runtime.GOOS == "windows" {
		return
	}
	go func() {
		for os.Getppid() == parentPID {
			time.Sleep(poll)
		}
		deadline := time.Now().Add(grace)
		for slashWorkerInFlight.Load() && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		observability.FlushBeforeHardExit(ctx, observability.ExitFlushTimeout)
		os.Exit(0)
	}()
}

// Starts and waits for bounded MCP discovery before the CLI snapshots its tools
func prepareSlashWorkerRuntime(ctx context.Context, env *tools.Env) {
	mcp.StartBackgroundDiscovery(ctx, env, slashWorkerLogger(), "slash-worker-mcp-discovery")
	mcp.WaitForDiscovery(ctx, env, nil, false)
}

// Lowercased command word without its slash
func slashBase(command string) string {
	cmd := strings.TrimPrefix(pystr.Strip(command), "/")
	if cmd == "" {
		return ""
	}
	return pystr.Lower(pystr.SplitWhitespace(cmd, 1)[0])
}

// Refuses a skill command before it prints its loading banner, a failed skill scan refusing nothing
func refuseSkillSlash(ctx context.Context, command string) (err error) {
	base := slashBase(command)
	if base == "" {
		return nil
	}
	var commands map[string]map[string]any
	func() {
		defer func() {
			if recover() != nil {
				commands = nil
			}
		}()
		commands = GetSkillCommands(ctx)
	}()
	if _, present := commands["/"+base]; present {
		return fmt.Errorf("skill command refused before process: /%s", base)
	}
	return nil
}

// Runs one command with every print captured, returning its ANSI stripped output and the harvested seed
func runSlashWorkerCommand(c *CLI, command string) (output string, seed any, err error) {
	cmd := pystr.Strip(command)
	if cmd == "" {
		return "", "", nil
	}
	if err := refuseSkillSlash(c.ctx, cmd); err != nil {
		return "", "", err
	}
	if !strings.HasPrefix(cmd, "/") {
		cmd = "/" + cmd
	}
	r, w, err := os.Pipe()
	if err != nil {
		return "", "", err
	}
	var buf strings.Builder
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(drained)
	}()
	captured := &cliui.Console{In: os.Stdin, Out: w, StdoutTTY: func() bool { return true },
		TermSize: func() (int, int, error) { return 25, 120, nil }}
	savedStdout, savedStderr, savedConsole := os.Stdout, os.Stderr, c.console
	os.Stdout, os.Stderr, c.console = w, w, captured
	restoreStreams := SetStreams(captured, w)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				if e, isErr := rec.(error); isErr {
					err = e
				} else {
					err = errors.New(pyrepr.Str(rec))
				}
			}
		}()
		c.processCommand(cmd)
	}()
	restoreStreams()
	os.Stdout, os.Stderr, c.console = savedStdout, savedStderr, savedConsole
	_ = w.Close()
	<-drained
	_ = r.Close()
	if err != nil {
		return "", "", err
	}
	output = tools.StripANSI(pystr.Rstrip(buf.String()))
	seed = c.pendingAgentSeed
	if !pyrepr.Truthy(seed) {
		seed = ""
	}
	c.pendingAgentSeed = nil
	return output, seed, nil
}

// Writes one JSON reply line to the real stdout
func slashWorkerReply(out io.Writer, pairs ...any) {
	encoded, err := pyjson.Dumps(pyjson.ObjectOf(pairs...))
	if err != nil {
		panic(err)
	}
	fmt.Fprint(out, encoded+"\n")
}

// Child main reading id and command lines from stdin and answering each with its output or error
func slashWorkerMain(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("slash_worker", flag.ContinueOnError)
	sessionKey := fs.String("session-key", "", "")
	model := fs.String("model", "", "")
	providerName := fs.String("provider", "", "")
	parentPID := fs.Int("parent-pid", 0, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	keySet := false
	fs.Visit(func(f *flag.Flag) { keySet = keySet || f.Name == "session-key" })
	if !keySet {
		fmt.Fprintln(os.Stderr, "slash_worker: error: the following arguments are required: --session-key")
		return 2
	}
	_ = os.Setenv("ICEBATH_SESSION_KEY", *sessionKey)
	_ = os.Setenv("ICEBATH_INTERACTIVE", "1")
	startLegacyParentWatchdog(ctx, *parentPID, 2*time.Second, 5*time.Second)
	_ = os.Setenv("ICEBATH_QUIET", "1")
	if _, err := provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: home.Dir(ctx)}); err != nil {
		slashWorkerLogger().Debug("slash worker dotenv load failed", "error", err)
	}
	cwd, _ := os.Getwd()
	slashWorkerPrepare(ctx, &tools.Env{Home: home.Dir(ctx), Cwd: cwd})
	realStdout := os.Stdout
	compact := true
	var c *CLI
	var buildErr error
	func() {
		devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			buildErr = err
			return
		}
		defer devnull.Close()
		savedStdout, savedStderr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = devnull, devnull
		restore := SetStreams(&cliui.Console{In: os.Stdin, Out: devnull}, devnull)
		defer func() {
			restore()
			os.Stdout, os.Stderr = savedStdout, savedStderr
		}()
		c, buildErr = slashWorkerNewCLI(ctx, CLIOptions{Model: *model, Provider: *providerName, Compact: &compact, Resume: *sessionKey})
	}()
	if buildErr != nil {
		fmt.Fprintln(os.Stderr, buildErr)
		return 1
	}
	c.slashMetricsOff = true
	c.slashWorker = true
	var recoveryTimes []float64
	reader := bufio.NewReader(os.Stdin)
	for {
		raw, readErr := reader.ReadString('\n')
		if raw == "" && readErr != nil {
			if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, syscall.EAGAIN) {
				break
			}
			if !tuigateway.HandleSpuriousEOF(&recoveryTimes, func(reason string) {
				fmt.Fprintf(os.Stderr, "[slash-worker] %s\n", reason)
			}) {
				break
			}
			continue
		}
		line := pystr.Strip(raw)
		if line == "" {
			continue
		}
		slashWorkerInFlight.Store(true)
		handleSlashWorkerLine(c, realStdout, line)
		slashWorkerInFlight.Store(false)
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					slashWorkerLogger().Debug(fmt.Sprintf("slash worker memory trim failed: %v", rec))
				}
			}()
			memory.TrimMemory(ctx, &tools.Env{Home: home.Dir(ctx)}, memory.TrimOptions{Reason: "slash worker command completion"})
		}()
	}
	return 0
}

// Parses one request line, runs it and writes its reply, a failure answering ok false with its text
func handleSlashWorkerLine(c *CLI, out io.Writer, line string) {
	var rid any
	req, err := pyjson.LoadsDict(line)
	if err == nil {
		rid = req.GetOr("id", nil)
		rawCommand := req.GetOr("command", "")
		command, isText := rawCommand.(string)
		if !isText && pyrepr.Truthy(rawCommand) {
			slashWorkerReply(out, "id", rid, "ok", false, "error", fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(rawCommand)))
			return
		}
		var output string
		var seed any
		output, seed, err = runSlashWorkerCommand(c, command)
		if err == nil {
			slashWorkerReply(out, "id", rid, "ok", true, "output", output, "seed", seed)
			return
		}
	}
	slashWorkerReply(out, "id", rid, "ok", false, "error", err.Error())
}
