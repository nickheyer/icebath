//go:build !windows

package proc

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Symbolic name of sig such as SIGKILL, false for a number this platform does not define
func SignalName(sig syscall.Signal) (string, bool) {
	name := unix.SignalName(sig)
	return name, name != ""
}

// Outcome decoded from the wait status, the signal kept apart from the exit code
func exitOf(state *os.ProcessState) Exit {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok {
		return waitExitOf(ws)
	}
	return Exit{Code: state.ExitCode()}
}

// Outcome of a raw wait status
func waitExitOf(ws syscall.WaitStatus) Exit {
	if ws.Signaled() {
		return Exit{Code: -1, Signal: ws.Signal()}
	}
	return Exit{Code: ws.ExitStatus()}
}
