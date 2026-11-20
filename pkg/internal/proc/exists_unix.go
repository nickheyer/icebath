//go:build !windows

package proc

import "syscall"

// Reports a live process for pid without signalling it, zombies counting as dead
func Exists(pid int) bool {
	if pid <= 0 || Zombie(pid) {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
