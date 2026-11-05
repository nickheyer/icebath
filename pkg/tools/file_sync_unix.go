//go:build !windows

package tools

import (
	"os"
	"os/signal"
	"syscall"
)

// Reports whether a process with the pid exists
func syncPidExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// Defers SIGINT until the returned restore runs, which re-delivers a deferred interrupt
func deferInterruptSignal(env *Env) func() {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGINT)
	return func() {
		signal.Stop(ch)
		select {
		case <-ch:
			toolsLogger(env).Debug("sync_back: SIGINT deferred until sync completes")
			syscall.Kill(os.Getpid(), syscall.SIGINT)
		default:
		}
	}
}
