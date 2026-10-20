//go:build !windows

package proc

import (
	"context"
	"os/exec"
	"syscall"
)

func group(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func kill(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func shellCommand(ctx context.Context, line string) *exec.Cmd {
	shell := "sh"
	if p, err := exec.LookPath("bash"); err == nil {
		shell = p
	}
	return exec.CommandContext(ctx, shell, "-c", line)
}
