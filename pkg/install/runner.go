package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Install options, the flags of the install scripts after renames
type Options struct {
	Channel         string
	Branch          string
	Commit          string
	Dir             string
	Home            string
	RepoURL         string
	Binary          string
	Version         string
	FromSelf        bool
	Stage           string
	Manifest        bool
	JSON            bool
	NonInteractive  bool
	IncludeDesktop  bool
	Verbose         bool
	SkipBrowser     bool
	SkipComputerUse bool
	InstallMethod   string
}

// Failure of one stage with its reason and closed failure class
type StageError struct {
	Reason string
	Class  string
}

func (e *StageError) Error() string { return e.Reason }

func fail(reason, class string) error { return &StageError{Reason: reason, Class: class} }

// Runs install stages in process, reporting every line and transition through the sink
type Runner struct {
	opts       Options
	sink       func(Event)
	color      bool
	quiet      bool
	home       string
	installDir string
	stage      string
	skipped    bool
	stdout     io.Writer
	stderr     io.Writer
	lastErr    error
	mu         sync.Mutex
}

// Runner for the options, the home and install dir resolved and exported like install.sh does
func NewRunner(opts Options, sink func(Event)) (*Runner, error) {
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	switch opts.Channel {
	case "", "stable", "main":
	default:
		return nil, fmt.Errorf("--channel must be stable or main, not %s", opts.Channel)
	}
	if opts.RepoURL == "" {
		opts.RepoURL = pystr.Strip(os.Getenv("ICEBATH_REPO_URL"))
	}
	if opts.RepoURL == "" {
		opts.RepoURL = DefaultRepoURL
	}
	if opts.Channel == "" {
		opts.Channel = pystr.Strip(os.Getenv("ICEBATH_INSTALL_CHANNEL"))
		switch opts.Channel {
		case "", "stable", "main":
		default:
			return nil, fmt.Errorf("--channel must be stable or main, not %s", opts.Channel)
		}
	}
	if opts.Home != "" {
		if err := os.Setenv(home.EnvHome, opts.Home); err != nil {
			return nil, err
		}
	}
	if opts.Version != "" && !strings.HasPrefix(opts.Version, "v") {
		opts.Version = "v" + opts.Version
	}
	h := Home()
	dir := opts.Dir
	if dir == "" {
		dir = os.Getenv("ICEBATH_INSTALL_DIR")
	}
	if dir == "" {
		dir = InstallRootOf(h)
	}
	if err := os.Setenv(home.EnvHome, h); err != nil {
		return nil, err
	}
	if opts.FromSelf && opts.Binary == "" {
		if exe, err := os.Executable(); err == nil && !samePath(exe, InstallBinaryPath(dir)) {
			opts.Binary = exe
		}
	}
	return &Runner{opts: opts, sink: sink, home: h, installDir: dir, stdout: os.Stdout, stderr: os.Stderr}, nil
}

// Install dir the runner installs into
func (r *Runner) InstallDir() string { return r.installDir }

func (r *Runner) emit(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sink(e)
}

func (r *Runner) line(stream LogStream, text string) { r.emit(LogEvent(r.stage, text, stream)) }

func (r *Runner) paint(code, glyph string) string {
	if !r.color {
		return glyph
	}
	return code + glyph + "\x1b[0m"
}

func (r *Runner) logInfo(text string)    { r.line(Stdout, r.paint("\x1b[0;36m", "→")+" "+text) }
func (r *Runner) logSuccess(text string) { r.line(Stdout, r.paint("\x1b[0;32m", "✓")+" "+text) }
func (r *Runner) logWarn(text string)    { r.line(Stdout, r.paint("\x1b[0;33m", "⚠")+" "+text) }
func (r *Runner) logError(text string)   { r.line(Stderr, r.paint("\x1b[0;31m", "✗")+" "+text) }

// Runs one stage and returns its result frame and exit status
func (r *Runner) RunStage(ctx context.Context, name string) (StageResult, int) {
	r.stage, r.skipped, r.lastErr = name, false, nil
	reason := ""
	if r.opts.NonInteractive && (name == "setup" || name == "gateway") {
		r.skipped = true
		return StageResult{Stage: name, OK: true, Skipped: true, Reason: strPtr("needs user input")}, 0
	}
	var err error
	switch name {
	case "prerequisites":
		err = r.stagePrerequisites(ctx)
	case "repository":
		err = r.stageRepository(ctx)
	case "config":
		err = r.stageConfig(ctx)
	case "products":
		err = r.stageProducts(ctx)
	case "runtimes":
		err = r.stageRuntimes(ctx)
	case "setup":
		err = r.stageSetup(ctx)
	case "gateway":
		err = r.stageGateway(ctx)
	case "desktop":
		r.opts.IncludeDesktop = true
		err = r.stageProducts(ctx)
	case "complete":
		err = r.stageComplete(ctx)
	default:
		reason = "unknown stage: " + name
		r.lastErr = fail(reason, "other")
		r.line(Stderr, reason)
		return StageResult{Stage: name, OK: false, Reason: strPtr(reason)}, 2
	}
	r.lastErr = err
	if err == nil {
		return StageResult{Stage: name, OK: true, Skipped: r.skipped}, 0
	}
	var se *StageError
	if errors.As(err, &se) {
		r.logError(se.Reason)
		reason = se.Reason
	} else {
		reason = err.Error()
		r.logError(reason)
	}
	if reason == "" {
		reason = "stage failed (exit 1)"
	}
	return StageResult{Stage: name, OK: false, Reason: strPtr(reason)}, 1
}

// Closed failure class of a stage error, other when it names none
func failureClass(err error) string {
	var se *StageError
	if errors.As(err, &se) && se.Class != "" {
		return se.Class
	}
	return "other"
}

// Refuses hosts the release has no build for, Termux included
func checkPlatform() error {
	if os.Getenv("TERMUX_VERSION") != "" || strings.Contains(os.Getenv("PREFIX"), "com.termux/files/usr") {
		return fail("Termux is installed from its APT repository, not install.sh: pkg install icebath-agent (setup: https://hermes-agent.nousresearch.com/docs/getting-started/termux)", "unsupported_platform")
	}
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		return fail("unsupported platform: "+runtime.GOOS+". On Windows use install.ps1.", "unsupported_platform")
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
	default:
		return fail(fmt.Sprintf("no release build for this platform (%s %s); Icebath does not support this host", runtime.GOOS, runtime.GOARCH), "unsupported_platform")
	}
	return nil
}

// Reports whether interactive runs collapse progress into one status line
func quietOutput(verbose bool, stdout *os.File) bool {
	if verbose || os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" || os.Getenv("ICEBATH_INSTALL_VERBOSE") != "" {
		return false
	}
	return isatty.IsTerminal(stdout.Fd())
}

// Writes log events to the process streams, the CLI form of the event sink
func cliSink(stdout, stderr io.Writer) func(Event) {
	return func(e Event) {
		if e.Type != EventLog {
			return
		}
		if e.Stream == Stderr {
			fmt.Fprintln(stderr, e.Line)
			return
		}
		fmt.Fprintln(stdout, e.Line)
	}
}

func (r *Runner) printBanner() {
	open, close := "", ""
	if r.color {
		open, close = "\x1b[0;35m\x1b[1m", "\x1b[0m"
	}
	fmt.Fprintf(r.stdout, "\n%s%s\n%s\n%s\n%s\n%s\n%s\n", open,
		"┌─────────────────────────────────────────────────────────┐",
		"│             ☤ Icebath Agent Installer                   │",
		"├─────────────────────────────────────────────────────────┤",
		"│  An open source AI agent by Nous Research.              │",
		"└─────────────────────────────────────────────────────────┘",
		close)
}

// Runs the install verb and returns its exit status, the manifest, one stage or the whole ladder
func Main(opts Options, stdout, stderr *os.File) int {
	if opts.Manifest {
		fmt.Fprintln(stdout, ManifestJSON(opts.IncludeDesktop))
		return 0
	}
	r, err := NewRunner(opts, cliSink(stdout, stderr))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	r.stdout, r.stderr = stdout, stderr
	r.color = isatty.IsTerminal(stdout.Fd()) && os.Getenv("NO_COLOR") == ""
	r.quiet = quietOutput(opts.Verbose, stdout)
	ctx := context.Background()
	if opts.Stage != "" {
		if err := checkPlatform(); err != nil {
			r.logError(err.Error())
			if opts.JSON {
				fmt.Fprintln(stdout, FrameJSON(false, opts.Stage, false, err.Error()))
			}
			return 1
		}
		res, code := r.RunStage(ctx, opts.Stage)
		if opts.JSON {
			reason := ""
			if res.Reason != nil {
				reason = *res.Reason
			}
			fmt.Fprintln(stdout, FrameJSON(res.OK, res.Stage, res.Skipped, reason))
		}
		return code
	}
	return r.runLadder(ctx)
}

// Exit status and signal of one noted installer signal
func signalCode(sig os.Signal) int {
	switch sig {
	case syscall.SIGHUP:
		return 129
	case syscall.SIGTERM:
		return 143
	}
	return 130
}

// Runs every stage in manifest order with one receipt per run, an INT cancelling the running stage
func (r *Runner) runLadder(ctx context.Context) int {
	started := time.Now().Unix()
	runID := newRunID()
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	var noted os.Signal
	var notedMu sync.Mutex
	interrupted := func(stage string) int {
		notedMu.Lock()
		sig := noted
		notedMu.Unlock()
		writeInstallReceipt(r.home, runID, started, "failed", stage, "interrupted")
		signal.Reset(sig)
		if p, err := os.FindProcess(os.Getpid()); err == nil && runtime.GOOS != "windows" {
			_ = p.Signal(sig)
		}
		return signalCode(sig)
	}
	if err := checkPlatform(); err != nil {
		r.logError(err.Error())
		writeInstallReceipt(r.home, runID, started, "failed", "prerequisites", failureClass(err))
		return 1
	}
	r.printBanner()
	for _, name := range StageNames() {
		notedMu.Lock()
		pending := noted
		notedMu.Unlock()
		if pending != nil {
			return interrupted(name)
		}
		stageCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			for {
				select {
				case sig := <-sigs:
					notedMu.Lock()
					noted = sig
					notedMu.Unlock()
					if sig == os.Interrupt {
						cancel()
					}
				case <-done:
					return
				}
			}
		}()
		res, code, err := r.runStageForLadder(stageCtx, name)
		close(done)
		cancel()
		notedMu.Lock()
		pending = noted
		notedMu.Unlock()
		if res.OK {
			if pending == os.Interrupt {
				notedMu.Lock()
				noted = nil
				notedMu.Unlock()
			}
			continue
		}
		if pending != nil {
			return interrupted(name)
		}
		writeInstallReceipt(r.home, runID, started, "failed", name, failureClass(err))
		return code
	}
	writeInstallReceipt(r.home, runID, started, "success", "none", "none")
	notedMu.Lock()
	pending := noted
	notedMu.Unlock()
	if pending != nil {
		return interrupted("complete")
	}
	r.printPathReloadHint()
	return 0
}

// One ladder stage with its error kept for the receipt class
func (r *Runner) runStageForLadder(ctx context.Context, name string) (StageResult, int, error) {
	res, code := r.RunStage(ctx, name)
	return res, code, r.lastErr
}

// Prints how to reach the command from the current shell when its inherited PATH lacks the user bin
func (r *Runner) printPathReloadHint() {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return
	}
	bin := filepath.Join(userHome, ".local", "bin")
	if runtime.GOOS == "windows" {
		return
	}
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if strings.TrimSuffix(entry, "/") == bin {
			return
		}
	}
	rc := ". ~/.profile"
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh":
		rc = "source ~/.zshrc"
	case "fish":
		rc = "source ~/.config/fish/config.fish"
	case "bash", ".", "":
		rc = "source ~/.bashrc"
	}
	r.logInfo("Reload your shell to use icebath: open a new terminal, or run: " + rc)
}
