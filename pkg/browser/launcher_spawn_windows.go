//go:build windows

package browser

import (
	"os"
	"syscall"
)

const (
	createNoWindow        = 0x08000000
	createNewProcessGroup = 0x00000200
)

// A hidden console in a new process group so a shared profile's Chromium outlives its launcher
func detachedSpawnAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow | createNewProcessGroup}
}

func popenReturnCode(state *os.ProcessState) int { return state.ExitCode() }
