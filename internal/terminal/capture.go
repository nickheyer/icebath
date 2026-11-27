package terminal

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Output of a program of this machine run to its end
type Captured struct {
	Code   int
	Stdout string
	Stderr string
}

// A program of this machine that ran past its time
var ErrTimedOut = errors.New("The command did not finish in time.")

// Runs a program of this machine with its output captured and no stdin, its whole process group killed once it runs past the timeout or
// ctx ends; env nil inherits this process's environment
func Capture(ctx context.Context, argv []string, timeout time.Duration, env []string) (*Captured, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	NewSession(cmd)
	cmd.Cancel = func() error {
		if err := SignalGroup(cmd.Process.Pid, true); err != nil && !errors.Is(err, ErrGone) {
			return cmd.Process.Kill()
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		return nil, ErrTimedOut
	}
	captured := &Captured{Stdout: strings.ToValidUTF8(stdout.String(), "�"), Stderr: strings.ToValidUTF8(stderr.String(), "�")}
	if err != nil {
		if _, exited := errors.AsType[*exec.ExitError](err); !exited {
			return nil, err
		}
		captured.Code = exitCode(cmd, err)
	}
	return captured, nil
}

// A program of this machine that exited non-zero
type ExitError struct {
	Argv   []string
	Code   int
	Stderr string
}

// Program and status first so a cut message keeps the cause, the whole command last
func (e *ExitError) Error() string {
	text := Join(e.Argv[:min(2, len(e.Argv))]) + " exited with status " + strconv.Itoa(e.Code)
	if detail := strings.TrimSpace(e.Stderr); detail != "" {
		text += ": " + detail
	}
	return text + " (command " + Join(e.Argv) + ")"
}

// Error of the program that ran argv, nil when it exited zero
func (c *Captured) Err(argv []string) error {
	if c.Code == 0 {
		return nil
	}
	return &ExitError{Argv: argv, Code: c.Code, Stderr: c.Stderr}
}
