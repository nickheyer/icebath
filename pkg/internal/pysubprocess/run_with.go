package pysubprocess

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// Keywords of one subprocess.run call with capture_output, nil Env inherits and nil Input reads the null device
type Options struct {
	Dir     string
	Env     []string
	Input   []byte
	Timeout time.Duration
	Check   bool
	Attr    *syscall.SysProcAttr
}

// Finished command with its exit status and both captured streams
type Completed struct {
	Args       []string
	Returncode int
	Stdout     []byte
	Stderr     []byte
}

// Runs argv like subprocess.run, a timeout kills the child and raises TimeoutExpired, a cancelled ctx returns its error
func RunWith(ctx context.Context, argv []string, o Options) (*Completed, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := ctx, context.CancelFunc(func() {})
	if o.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, o.Timeout)
	}
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.SysProcAttr = o.Dir, o.Env, o.Attr
	if o.Input != nil {
		cmd.Stdin = bytes.NewReader(o.Input)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	runErr := cmd.Run()
	res := &Completed{Args: argv, Stdout: out.Bytes(), Stderr: errOut.Bytes()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if o.Timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return res, &TimeoutExpired{Cmd: argv, Timeout: int(o.Timeout / time.Second), Stdout: res.Stdout, Stderr: res.Stderr}
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.Returncode = returncode(exitErr)
		if o.Check {
			return res, &CalledProcessError{Cmd: argv, Returncode: res.Returncode, Stdout: res.Stdout, Stderr: res.Stderr}
		}
		return res, nil
	}
	return res, runErr
}
