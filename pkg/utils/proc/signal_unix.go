//go:build !windows

package proc

import "syscall"

// Asks pid to exit with SIGTERM
func Terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
