//go:build !windows

package kanban

import (
	"os"
	"syscall"
)

// Raw POSIX wait status of a finished worker, the stopped marker when no status was recovered
func rawWaitStatus(state *os.ProcessState, _ error) int {
	if state == nil {
		return 0x7F
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok {
		return int(ws)
	}
	return waitStatusFromReturncode(state.ExitCode())
}
