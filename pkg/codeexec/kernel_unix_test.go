//go:build !windows

package codeexec

import "syscall"

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func killPid(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
