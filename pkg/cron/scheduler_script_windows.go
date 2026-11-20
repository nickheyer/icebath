//go:build windows

package cron

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

const (
	cronCreateNoWindow        = 0x08000000
	cronCreateNewProcessGroup = 0x00000200
)

// Hidden console in a new process group
func cronScriptSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: cronCreateNoWindow | cronCreateNewProcessGroup, HideWindow: true}
}

// Force kills the script tree through taskkill, a failing taskkill killing the process alone
func terminateCronScriptGroup(p *cronScriptProcess) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(p.cmd.Process.Pid), "/T", "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: cronCreateNoWindow, HideWindow: true}
	if _, err := cmd.CombinedOutput(); err != nil {
		if _, isExit := err.(*exec.ExitError); !isExit || ctx.Err() != nil {
			p.cmd.Process.Kill()
		}
	}
}

// Popen returncode of the exited script
func cronScriptReturnCode(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	return state.ExitCode()
}
