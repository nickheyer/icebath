//go:build !windows

package proc

import (
	"math"
	"syscall"
)

// Reports a live process for pid without signalling it, zombies counting as dead and a pid outside the 32-bit pid_t range naming no process
func Exists(pid int) bool {
	if pid <= 0 || pid > math.MaxInt32 || Zombie(pid) {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
