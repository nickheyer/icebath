//go:build !windows

package gateway

import (
	"errors"
	"syscall"
)

// Sends SIGKILL to pid
func cgroupSigkill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

// Whether a kill failure is the vanished or forbidden pid the reaper steps over
func cgroupKillSkippable(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}
