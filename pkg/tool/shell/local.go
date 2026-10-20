package shell

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Script run by the shell so the final directory reaches fd 3 however the command ends
const wrapper = `trap 'pwd -P >&3' EXIT
eval "$1"`

// Runner using the local shell in its own process group with stdin closed
type Local struct {
	// bash when found on PATH, otherwise sh
	Shell string
}

func (l Local) shell() string {
	if l.Shell != "" {
		return l.Shell
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "sh"
}

func (l Local) Run(ctx context.Context, cmd Command) (Output, error) {
	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}
	shell := l.shell()
	c := exec.CommandContext(ctx, shell, "-c", wrapper, shell, cmd.Line)
	c.Dir = cmd.Dir
	c.Env = append(os.Environ(), cmd.Env...)
	c.Stdout = orDiscard(cmd.Stdout)
	c.Stderr = orDiscard(cmd.Stderr)
	r, w, err := os.Pipe()
	if err != nil {
		return Output{}, err
	}
	c.ExtraFiles = []*os.File{w}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 2 * time.Second
	start := time.Now()
	if err := c.Start(); err != nil {
		w.Close()
		r.Close()
		return Output{}, err
	}
	w.Close()
	dirCh := make(chan string, 1)
	go func() {
		defer r.Close()
		line, _ := bufio.NewReader(r).ReadString('\n')
		dirCh <- strings.TrimSuffix(line, "\n")
	}()
	waitErr := c.Wait()
	out := Output{Dir: cmd.Dir, Duration: time.Since(start), TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	select {
	case dir := <-dirCh:
		if dir != "" {
			out.Dir = dir
		}
	case <-time.After(c.WaitDelay):
	}
	out.ExitCode = exitCode(c, waitErr)
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

// Shell style status, 128 plus the signal number when the process was killed
func exitCode(c *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	if ws, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return c.ProcessState.ExitCode()
}
