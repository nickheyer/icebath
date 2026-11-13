package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// How long a killed or finished child's pipes may stay open before they are closed under the reader
const drainBound = time.Second

// How a finished process ended, by its own exit code or by a signal
type Exit struct {
	// Exit code, -1 when a signal ended the process
	Code int
	// Signal that ended the process, zero when it exited by itself and always zero on Windows
	Signal syscall.Signal
}

// Outcome recorded in state, an unstarted or unwaited command reading as code -1
func ExitOf(state *os.ProcessState) Exit {
	if state == nil {
		return Exit{Code: -1}
	}
	return exitOf(state)
}

// True for a zero exit code without a signal
func (e Exit) Success() bool { return e.Signal == 0 && e.Code == 0 }

// Exit code, or the negated signal number for a signalled process, the single integer status records store
func (e Exit) Status() int {
	if e.Signal != 0 {
		return -int(e.Signal)
	}
	return e.Code
}

// Words describing the outcome
func (e Exit) String() string {
	if e.Signal != 0 {
		if name, ok := SignalName(e.Signal); ok {
			return fmt.Sprintf("was killed by %s (signal %d)", name, int(e.Signal))
		}
		return fmt.Sprintf("was killed by signal %d", int(e.Signal))
	}
	return fmt.Sprintf("exited with status %d", e.Code)
}

// A command that finished without success, with what it wrote to stderr when that was captured
type ExitError struct {
	Args   []string
	Exit   Exit
	Stderr []byte
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("command %q %s", strings.Join(e.Args, " "), e.Exit)
}

// Finished command with its outcome and the output Run captured
type Result struct {
	Args []string
	// Standard output when the caller left it unset, nil otherwise
	Stdout []byte
	// Standard error when the caller left it unset, nil otherwise
	Stderr []byte
	Exit   Exit
}

// ExitError for an unsuccessful outcome, nil for success
func (r Result) Err() error {
	if r.Exit.Success() {
		return nil
	}
	return &ExitError{Args: r.Args, Exit: r.Exit, Stderr: r.Stderr}
}

// Outcome of a contained child's wait, its own Wait result kept apart from a failure to end its leftovers
type treeWait struct{ wait, end error }

// Runs cmd contained, capturing unset output, killing the tree when ctx ends and its group or job leftovers after exit
func Run(ctx context.Context, cmd *exec.Cmd) (Result, error) {
	res := Result{Args: append([]string(nil), cmd.Args...)}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	var stdout, stderr *bytes.Buffer
	if cmd.Stdout == nil {
		stdout = new(bytes.Buffer)
		cmd.Stdout = stdout
	}
	if cmd.Stderr == nil {
		stderr = new(bytes.Buffer)
		cmd.Stderr = stderr
	}
	Hide(cmd)
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = drainBound
	}
	tree, err := StartTree(cmd)
	if err != nil {
		return res, err
	}
	done := make(chan treeWait, 1)
	go func() {
		w, e := tree.wait()
		done <- treeWait{w, e}
	}()
	var out treeWait
	var stopErr error
	select {
	case out = <-done:
	case <-ctx.Done():
		select {
		case out = <-done:
		default:
			stopErr = context.Cause(ctx)
			if kerr := tree.Kill(); kerr != nil && !errors.Is(kerr, ErrNotFound) {
				stopErr = errors.Join(stopErr, kerr)
			}
			out = <-done
		}
	}
	if stdout != nil {
		res.Stdout = stdout.Bytes()
	}
	if stderr != nil {
		res.Stderr = stderr.Bytes()
	}
	res.Exit = ExitOf(cmd.ProcessState)
	if stopErr != nil {
		return res, fmt.Errorf("command %q stopped: %w", strings.Join(cmd.Args, " "), errors.Join(stopErr, out.end))
	}
	var exitErr *exec.ExitError
	if out.wait != nil && !errors.As(out.wait, &exitErr) && !errors.Is(out.wait, exec.ErrWaitDelay) {
		return res, errors.Join(out.wait, out.end)
	}
	return res, out.end
}
