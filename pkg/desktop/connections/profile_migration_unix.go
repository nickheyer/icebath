//go:build !windows

package connections

import "syscall"

// Signal zero existence probe, a process this user cannot signal counting as absent
func signalZero(pid int) bool { return syscall.Kill(pid, 0) == nil }
