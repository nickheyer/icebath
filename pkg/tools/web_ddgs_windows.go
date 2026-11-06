//go:build windows

package tools

import (
	"os"
	"syscall"
)

// Own process group for the worker, CREATE_NEW_PROCESS_GROUP
func ddgsWorkerAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// TerminateProcess like Popen.terminate on Windows
func ddgsTerminate(p *os.Process) error { return p.Kill() }
