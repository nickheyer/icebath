//go:build windows

package kanban

import "os"

// Wait status layout of a finished worker exit code, the stopped marker when no status was recovered
func rawWaitStatus(state *os.ProcessState, _ error) int {
	if state == nil {
		return 0x7F
	}
	return waitStatusFromReturncode(state.ExitCode())
}
