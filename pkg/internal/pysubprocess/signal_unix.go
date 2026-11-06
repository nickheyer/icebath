//go:build !windows

package pysubprocess

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// Popen returncode, the negated signal number for a child killed by a signal
func returncode(e *exec.ExitError) int {
	if ws, ok := e.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return e.ExitCode()
}

func signalName(n int) (string, bool) {
	name := unix.SignalName(syscall.Signal(n))
	return name, name != ""
}
