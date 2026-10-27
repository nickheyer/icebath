//go:build !windows

package proc

import (
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

func killPID(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
