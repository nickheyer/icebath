//go:build !windows

package proc

import (
	"os"
	"os/exec"
	"syscall"
)

// Exit code of a finished process, the negated number of the signal that ended one a signal ended
func ExitCode(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -int(status.Signal())
	}
	return state.ExitCode()
}

// Keeps a child from opening a console window of its own, which only Windows gives it
func HideWindow(*exec.Cmd) {}
