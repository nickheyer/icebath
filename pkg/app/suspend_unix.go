//go:build !windows

package app

import "syscall"

// Sends SIGTSTP to the process group, os.kill(0, SIGTSTP)
func suspendProcessGroup() error { return syscall.Kill(0, syscall.SIGTSTP) }
