//go:build windows

package tuiserver

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Whether pid names a live process
func hostPIDAlive(pid int) bool { return pid > 0 && proc.Exists(pid) }

// Terminates the process, the TerminateProcess both SIGTERM and SIGKILL map to, false when it is gone or the call failed
func hostSignalPID(pid int, kill bool, label string) bool {
	p, err := os.FindProcess(pid)
	if err == nil {
		err = p.Kill()
	}
	if err == nil {
		return true
	}
	if !errors.Is(err, os.ErrProcessDone) {
		logger().Debug(fmt.Sprintf("failed to %s compute host pid=%d: %s", label, pid, err))
	}
	return false
}

// Process attributes of the child, a new session being a POSIX only spawn option
func hostNewSessionAttr() *syscall.SysProcAttr { return nil }

// Command line of a pid, procfs being absent here
func hostProcCmdline(pid int) string { return "" }

// One write of data through an append only handle, the kernel appending since CRT O_APPEND is seek then write
func hostAppendWrite(path string, data []byte) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.FILE_APPEND_DATA,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var written uint32
	return windows.WriteFile(handle, data, &written, nil)
}
