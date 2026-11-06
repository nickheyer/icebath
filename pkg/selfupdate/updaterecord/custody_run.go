package updaterecord

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const custodyDrain = 5 * time.Second

var (
	gitNoDetach   = []string{"-c", "gc.autoDetach=false", "-c", "maintenance.auto=false"}
	mutatorConfig = []string{"-c", "core.fsmonitor=false", "-c", "credential.helper=", "-c", "core.hooksPath=" + os.DevNull}
	localMutators = map[string]bool{
		"add": true, "am": true, "apply": true, "checkout": true, "checkout-index": true, "cherry-pick": true,
		"clean": true, "commit": true, "gc": true, "merge": true, "mv": true, "pack-objects": true,
		"read-tree": true, "rebase": true, "reset": true, "restore": true, "revert": true, "rm": true,
		"stash": true, "switch": true, "update-index": true, "update-ref": true, "symbolic-ref": true,
		"tag": true, "branch": true, "worktree": true,
	}
	globalWithValue = map[string]bool{
		"-c": true, "-C": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--exec-path": true,
		"--config-env": true, "--super-prefix": true, "--attr-source": true, "--list-cmds": true,
	}
	custodyRetry = "Run {command} again from a regular terminal, outside any sandbox or tool that confines the processes it starts."
)

// Captured outcome of one git child with text mode output
type GitResult struct {
	Args       []string
	ReturnCode int
	Stdout     string
	Stderr     string
}

// Git child that outlived its timeout and was killed with its tree
type GitTimeout struct {
	Args    []string
	Timeout time.Duration
}

func (e *GitTimeout) Error() string {
	args := make([]any, len(e.Args))
	for i, a := range e.Args {
		args[i] = a
	}
	return fmt.Sprintf("Command '%s' timed out after %v seconds", pyrepr.Repr(args), e.Timeout.Seconds())
}

// Windows refused to bind an update child to the update job so the child never ran
type CustodyRefused struct{ Reason string }

func (e *CustodyRefused) Error() string {
	return e.Reason + " " + strings.ReplaceAll(custodyRetry, "{command}", "the command")
}

var custodyRun struct {
	sync.Mutex
	ran       bool
	refused   *CustodyRefused
	unchanged bool
}

// First non option argument after the git executable, skipping global options and their values
func GitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if globalWithValue[args[i]] {
			i++
		} else if !strings.HasPrefix(args[i], "-") {
			return args[i]
		}
	}
	return ""
}

// Reports whether args run a git subcommand that writes this machine's worktree, index or refs
func IsLocalMutator(args []string) bool { return localMutators[GitSubcommand(args)] }

// Joins the git command and args with the custody config inserted before the subcommand
func GitArgv(gitCmd, args []string) []string {
	extra := append([]string{}, gitNoDetach...)
	if IsLocalMutator(append(append([]string{}, gitCmd[1:]...), args...)) {
		extra = append(extra, mutatorConfig...)
	}
	return append(append(append([]string{}, gitCmd...), extra...), args...)
}

func checkoutHeld() bool {
	checkout.Lock()
	defer checkout.Unlock()
	return checkout.held != nil
}

// Whether the open receipt already recorded the apply stage, fail closed on an unreadable one
func pastCommit(ctx context.Context) bool {
	st := stateOf(ctx)
	st.mu.Lock()
	current := st.current
	st.mu.Unlock()
	if current == nil || current.Data == nil {
		return false
	}
	stages, _ := current.Data.Get("stages")
	for _, stage := range listItems(orEmptyList(stages)) {
		if mapGet(stage, "name") == "apply" {
			return true
		}
	}
	return false
}

func refuseChild(ctx context.Context, argv []string, cause error) *CustodyRefused {
	return RefuseCustodyChild(ctx, argv, cause, false)
}

// Refusal for argv, logged and receipted unless the caller already did, kept as this run's first refusal
func RefuseCustodyChild(ctx context.Context, argv []string, cause any, receipted bool) *CustodyRefused {
	name := "child"
	if len(argv) > 0 {
		name = filepath.Base(argv[0])
	}
	exc := &CustodyRefused{Reason: fmt.Sprintf("Windows would not put `%s` in this update's process job (%v), so it was not run: a checkout writer outside the job could outlive a killed update and write the checkout after its lock is released.", name, cause)}
	head := argv
	if len(head) > 2 {
		head = head[:2]
	}
	detail := exc.Reason + ": " + strings.Join(head, " ")
	if !receipted {
		slog.Warn("Refused update child: " + detail)
		RecordStep(ctx, "update_custody", false, detail)
	}
	custodyRun.Lock()
	if custodyRun.refused == nil {
		custodyRun.refused = exc
		custodyRun.unchanged = !custodyRun.ran && !pastCommit(ctx)
	}
	custodyRun.Unlock()
	return exc
}

// Text the failure path prints for this run's refused update child, nil when none was refused
func RefusalNotice(command string) *string {
	custodyRun.Lock()
	exc, unchanged := custodyRun.refused, custodyRun.unchanged
	custodyRun.Unlock()
	if exc == nil {
		return nil
	}
	outcome := "Update steps before it had already run."
	if unchanged {
		outcome = "Nothing was changed: no update step had run yet."
	}
	text := "✗ `" + command + "` stopped: " + exc.Reason + "\n  " + outcome + "\n  " +
		strings.ReplaceAll(custodyRetry, "{command}", "`"+command+"`")
	return &text
}

// Runs a read only git query in the updater's custody, refusing an argv that writes the checkout
func RunGitQuery(ctx context.Context, gitCmd, args []string, timeout time.Duration) (*GitResult, error) {
	argv := GitArgv(gitCmd, args)
	if IsLocalMutator(argv[1:]) {
		return nil, fmt.Errorf("git %s writes the checkout and runs only through the custody mutator runner", GitSubcommand(argv[1:]))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return runGitPlatform(ctx, argv, timeout, checkoutHeld())
}

type childSpawn struct {
	attr *syscall.SysProcAttr
	// Binds the started child to its custody and returns the dismiss callback for after the wait
	started func(cmd *exec.Cmd) (func(), error)
	// Kills the child and what it started once its timeout fires or its pipes stay open
	killTree func(cmd *exec.Cmd)
	// Kills the child when the caller's context ends the wait early
	interrupt func(cmd *exec.Cmd)
}

// Runs argv with null stdin and captured text output, killing its tree and draining when it times out
func runCaptured(ctx context.Context, argv []string, timeout time.Duration, spawn childSpawn) (*GitResult, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = spawn.attr
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return nil, err
	}
	var dismiss func()
	if spawn.started != nil {
		if dismiss, err = spawn.started(cmd); err != nil {
			outR.Close()
			errR.Close()
			_ = cmd.Wait()
			return nil, err
		}
	}
	var stdout, stderr []byte
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { stdout, _ = io.ReadAll(outR); readers.Done() }()
	go func() { stderr, _ = io.ReadAll(errR); readers.Done() }()
	var reaped sync.Mutex
	exited := false
	done := make(chan error, 1)
	go func() {
		readers.Wait()
		werr := cmd.Wait()
		reaped.Lock()
		exited = true
		reaped.Unlock()
		done <- werr
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		reaped.Lock()
		if !exited {
			spawn.killTree(cmd)
		}
		reaped.Unlock()
		select {
		case <-done:
		case <-time.After(custodyDrain):
		}
		if dismiss != nil {
			dismiss()
		}
		return nil, &GitTimeout{Args: argv, Timeout: timeout}
	case <-ctx.Done():
		reaped.Lock()
		if !exited {
			spawn.interrupt(cmd)
		}
		reaped.Unlock()
		if dismiss != nil {
			dismiss()
		}
		return nil, ctx.Err()
	}
	outR.Close()
	errR.Close()
	if dismiss != nil {
		dismiss()
	}
	code := cmd.ProcessState.ExitCode()
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		code = -int(status.Signal())
	}
	return &GitResult{Args: argv, ReturnCode: code, Stdout: decodeText(stdout), Stderr: decodeText(stderr)}, nil
}

func decodeText(raw []byte) string { return pystr.TranslateNewlines(pystr.DecodeUTF8Replace(raw)) }

func killTreeOf(cmd *exec.Cmd) { proc.KillTree(cmd.Process.Pid) }

func killChild(cmd *exec.Cmd) { _ = cmd.Process.Kill() }

// Records that an update child already ran in custody, so a later refusal says steps had run
func MarkCustodyRan() {
	custodyRun.Lock()
	custodyRun.ran = true
	custodyRun.Unlock()
}

// Reports whether this process holds or joined the checkout lock
func CheckoutHeld() bool { return checkoutHeld() }
