//go:build !windows

package harnesshost

import (
	"os/signal"
	"syscall"
)

// Ends the process the way Python does after an unhandled KeyboardInterrupt, by SIGINT with its default action
func exitInterrupted() int {
	signal.Reset(syscall.SIGINT)
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	return 128 + int(syscall.SIGINT)
}
