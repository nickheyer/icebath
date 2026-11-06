//go:build !windows

package tools

import (
	"os"
	"syscall"
)

// Own session for the worker, start_new_session
func ddgsWorkerAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// Sends SIGTERM like Popen.terminate
func ddgsTerminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
