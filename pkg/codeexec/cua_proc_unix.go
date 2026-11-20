//go:build !windows

package codeexec

import (
	"os"
	"os/exec"
	"syscall"
)

// Signal number that ended the child, the negated returncode subprocess reports
func signalNumber(e *exec.ExitError) int {
	if ws, ok := e.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return int(ws.Signal())
	}
	return 1
}

// Sends SIGTERM like Popen.terminate
func terminateProcess(p *os.Process) { _ = p.Signal(syscall.SIGTERM) }
