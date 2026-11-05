//go:build !windows

package provider

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

	"golang.org/x/sys/unix"
)

// Runs the helper through sh -c with the key as data in ICEBATH_SECRET_KEY, stdout or false
func runSecretHelper(command, secretKey string, timeout time.Duration, maxOutput int, environ *SourceEnvironment) (string, bool) {
	env := SourceChildEnv(environ)
	env["ICEBATH_SECRET_KEY"] = secretKey
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = envList(env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		errno := "unknown"
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			if en, ok := pathErr.Err.(syscall.Errno); ok {
				errno = fmt.Sprint(int(en))
			}
		}
		commandSecretLog("helper failed to spawn; resolving no value: errno=" + errno)
		return "", false
	}
	err := cmd.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		commandSecretLog(fmt.Sprintf("helper timed out after %gs; resolving no value", timeout.Seconds()))
		return "", false
	}
	if err != nil {
		code, signame := "?", "none"
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				if signame = unix.SignalName(status.Signal()); signame == "" {
					signame = fmt.Sprint(int(status.Signal()))
				}
			} else {
				code = fmt.Sprint(exitErr.ExitCode())
			}
		}
		commandSecretLog(fmt.Sprintf("helper failed; resolving no value: code=%s signal=%s", code, signame))
		return "", false
	}
	if stdout.Len() > maxOutput {
		commandSecretLog(fmt.Sprintf("helper output exceeded the %d-byte cap; resolving no value", maxOutput))
		return "", false
	}
	return strings.ToValidUTF8(stdout.String(), "�"), true
}
