//go:build !windows

package cron

import (
	"os"
	"syscall"
	"time"
)

// New session so the whole script tree shares one process group
func cronScriptSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// TERMs the script process group then KILLs it when any member survived the grace second
func terminateCronScriptGroup(p *cronScriptProcess) {
	pgid, err := syscall.Getpgid(p.cmd.Process.Pid)
	if err != nil {
		return
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		return
	}
	p.wait(time.Second)
	if err := syscall.Kill(-pgid, 0); err != nil {
		return
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
}

// Popen returncode, negative signal number for a signalled child
func cronScriptReturnCode(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return state.ExitCode()
}
