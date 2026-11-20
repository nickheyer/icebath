//go:build !windows

package browser

import (
	"os"
	"syscall"
)

// A new session so a shared profile's Chromium outlives its launcher for its other holders
func detachedSpawnAttrs() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

func popenReturnCode(state *os.ProcessState) int {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return state.ExitCode()
}
