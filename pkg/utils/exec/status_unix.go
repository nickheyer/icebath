//go:build unix

package executil

import (
	"os"
	"strconv"
	"syscall"
)

// Exit code of a finished process, the negated signal number when a signal ended it, -1 for a nil state
func ExitCode(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return state.ExitCode()
}

// Wait status text of an exit code, a negative one naming the signal
func statusText(code int) string {
	if code < 0 {
		return "signal: " + syscall.Signal(-code).String()
	}
	return "exit status " + strconv.Itoa(code)
}
