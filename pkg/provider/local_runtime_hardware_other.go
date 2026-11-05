//go:build !windows

package provider

import (
	"errors"
	"os/exec"
	"syscall"
)

// GlobalMemoryStatusEx exists only on Windows
func windowsMemoryStatus() *windowsMemoryStatusEx { return nil }

// Console windows exist only on Windows
func hideChildWindow(*exec.Cmd) {}

// Whether a process with pid exists, a permission refusal still counting as existing
func processPIDExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
