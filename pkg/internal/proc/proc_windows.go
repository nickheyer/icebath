//go:build windows

package proc

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
)

func group(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

func kill(cmd *exec.Cmd) error {
	err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	if err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

func shellCommand(ctx context.Context, line string) *exec.Cmd {
	if p, err := exec.LookPath("bash"); err == nil {
		return exec.CommandContext(ctx, p, "-c", line)
	}
	shell := "powershell"
	if p, err := exec.LookPath("pwsh"); err == nil {
		shell = p
	}
	return exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-Command", line)
}
