package executil

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// How long output is still read once the command exits or is killed while something it started holds its pipes open
const waitDelay = 2 * time.Second

var errNoCommand = errors.New("no command to run")

// Settings of one Run: nil Env inherits the environment, nil Input reads the null device, a positive Timeout kills the command once it passes, RequireSuccess makes a non-zero exit a *StatusError
type Options struct {
	Dir            string
	Env            []string
	Input          []byte
	Timeout        time.Duration
	RequireSuccess bool
	Attr           *syscall.SysProcAttr
}

// Finished command with its exit code, the negated signal number when a signal ended it on Unix and -1 when it never ran, and both captured streams
type Completed struct {
	Args     []string
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Runs argv with both streams captured; the result comes back even beside an error, an expired Timeout kills the command and gives a *TimeoutError, a done ctx gives the ctx error
func Run(ctx context.Context, argv []string, o Options) (*Completed, error) {
	res := &Completed{Args: argv, ExitCode: -1}
	if len(argv) == 0 {
		return res, errNoCommand
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx := ctx
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.SysProcAttr = o.Dir, o.Env, o.Attr
	if o.Input != nil {
		cmd.Stdin = bytes.NewReader(o.Input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var cut atomic.Bool
	cmd.Cancel = func() error {
		err := kill(cmd)
		cut.Store(!errors.Is(err, os.ErrProcessDone))
		return err
	}
	cmd.WaitDelay = waitDelay
	err := cmd.Run()
	res.Stdout, res.Stderr = stdout.Bytes(), stderr.Bytes()
	if cmd.ProcessState != nil {
		res.ExitCode = ExitCode(cmd.ProcessState)
	}
	if cut.Load() || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, ctxErr
		}
		if runCtx.Err() != nil {
			return res, &TimeoutError{Args: argv, Timeout: o.Timeout, Stdout: res.Stdout, Stderr: res.Stderr}
		}
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		return res, nil
	case errors.As(err, &exitErr):
		if o.RequireSuccess {
			return res, &StatusError{Args: argv, ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}
		}
		return res, nil
	}
	return res, err
}
