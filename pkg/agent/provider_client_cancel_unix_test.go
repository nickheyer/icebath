//go:build !windows

package agent

import (
	"os"
	"syscall"
)

// Terminates a process the way Popen.terminate does on POSIX
func terminateProcess(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
