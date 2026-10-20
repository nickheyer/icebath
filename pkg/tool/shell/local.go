package shell

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Runner using the local shell in its own process group with stdin closed
type Local struct {
	// bash when found on PATH, otherwise the platform shell
	Shell string
}

func (l Local) Run(ctx context.Context, cmd Command) (Output, error) {
	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}
	dirFile, err := os.CreateTemp("", "icebath-cwd-*")
	if err != nil {
		return Output{}, err
	}
	dirFile.Close()
	defer os.Remove(dirFile.Name())
	shell := l.shell()
	c := exec.CommandContext(ctx, shell, l.args(shell, cmd.Line, dirFile.Name())...)
	c.Dir = cmd.Dir
	c.Env = append(os.Environ(), cmd.Env...)
	c.Stdout = orDiscard(cmd.Stdout)
	c.Stderr = orDiscard(cmd.Stderr)
	proc.Group(c)
	c.Cancel = func() error { return proc.Kill(c) }
	c.WaitDelay = 2 * time.Second
	start := time.Now()
	if err := c.Start(); err != nil {
		if ctx.Err() != nil {
			return Output{Dir: cmd.Dir, ExitCode: 137, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}, nil
		}
		return Output{}, err
	}
	waitErr := c.Wait()
	out := Output{Dir: cmd.Dir, Duration: time.Since(start), TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	if data, err := os.ReadFile(dirFile.Name()); err == nil {
		if dir := strings.TrimSpace(string(data)); dir != "" && filepath.IsAbs(dir) {
			out.Dir = dir
		}
	}
	out.ExitCode = exitCode(c, waitErr, ctx.Err() != nil)
	if waitErr != nil && !isExit(waitErr) {
		return out, waitErr
	}
	return out, nil
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

func isExit(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}
