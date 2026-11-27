//go:build !windows

package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/internal/secrets"
)

// Runs the helper through sh -c in a session of its own, killed with everything it started at the timeout; its stdout, false after
// logging only how it failed, never what it printed
func helper(ctx context.Context, command string, timeout time.Duration, env map[string]string) (string, bool) {
	child := maps.Clone(env)
	if child == nil {
		child = map[string]string{}
	}
	child["ICEBATH_SECRET_KEY"] = ""
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = secrets.Pairs(child)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		errno := "unknown"
		if pathErr, ok := errors.AsType[*os.PathError](err); ok {
			if en, ok := pathErr.Err.(syscall.Errno); ok {
				errno = fmt.Sprint(int(en))
			}
		}
		slog.Warn("[secrets:command] helper failed to spawn; resolving no value: errno=" + errno)
		return "", false
	}
	err := cmd.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		slog.Warn(fmt.Sprintf("[secrets:command] helper timed out after %gs; resolving no value", timeout.Seconds()))
		return "", false
	}
	if err != nil {
		code, signal := "?", "none"
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				if signal = unix.SignalName(status.Signal()); signal == "" {
					signal = fmt.Sprint(int(status.Signal()))
				}
			} else {
				code = fmt.Sprint(exit.ExitCode())
			}
		}
		slog.Warn(fmt.Sprintf("[secrets:command] helper failed; resolving no value: code=%s signal=%s", code, signal))
		return "", false
	}
	if stdout.Len() > maxOutput {
		slog.Warn(fmt.Sprintf("[secrets:command] helper output exceeded the %d-byte cap; resolving no value", maxOutput))
		return "", false
	}
	return strings.ToValidUTF8(stdout.String(), "�"), true
}
