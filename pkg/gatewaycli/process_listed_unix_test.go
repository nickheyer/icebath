//go:build unix

package gatewaycli

import "syscall"

// Reports the pid still present in the process table, a zombie included
func processListed(pid int) bool { return syscall.Kill(pid, 0) == nil }
