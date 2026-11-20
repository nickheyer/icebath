//go:build windows

package lsp

import (
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/windows"
)

// Suppresses the console window a child would flash from a console less host
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}

// Gives the child its own process group with no console window
func newSession(cmd *exec.Cmd) {
	hideWindow(cmd)
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// Python os.access with X_OK, true for every existing file on Windows
func accessExec(p string) bool { return osPathExists(p) }

var pipeSeq atomic.Uint64

// Duplex overlapped named pipe for a child's stdin, so a read on our end reports the child closing it
func stdinPipe() (child, ours *os.File, err error) {
	name := fmt.Sprintf(`\\.\pipe\icebath-lsp-%d-%d`, os.Getpid(), pipeSeq.Add(1))
	n16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, nil, err
	}
	h, err := windows.CreateNamedPipe(n16, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT, 1, 64*1024, 64*1024, 0, nil)
	if err != nil {
		return nil, nil, err
	}
	c, err := windows.CreateFile(n16, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		windows.CloseHandle(h)
		return nil, nil, err
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(c)
		windows.CloseHandle(h)
		return nil, nil, err
	}
	defer windows.CloseHandle(ev)
	ov := windows.Overlapped{HEvent: ev}
	switch err := windows.ConnectNamedPipe(h, &ov); err {
	case nil, windows.ERROR_PIPE_CONNECTED:
	case windows.ERROR_IO_PENDING:
		var n uint32
		if err := windows.GetOverlappedResult(h, &ov, &n, true); err != nil {
			windows.CloseHandle(c)
			windows.CloseHandle(h)
			return nil, nil, err
		}
	default:
		windows.CloseHandle(c)
		windows.CloseHandle(h)
		return nil, nil, err
	}
	return os.NewFile(uintptr(c), name), os.NewFile(uintptr(h), name), nil
}

// Calls onClose once when the child closes its end of the duplex stdin pipe, until stopped
func watchPipeClose(f *os.File, onClose func()) (stop func()) {
	var stopped atomic.Bool
	go func() {
		buf := make([]byte, 16)
		for {
			if _, err := f.Read(buf); err != nil {
				if !stopped.Load() {
					onClose()
				}
				return
			}
		}
	}()
	return func() { stopped.Store(true) }
}

// The stdin watcher reports reader loss, so a process exit alone does not close the pipe
const exitClosesStdin = false
