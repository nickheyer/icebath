//go:build windows

package tools

import (
	"fmt"
	"os"
	"sync/atomic"

	"golang.org/x/sys/windows"
)

var pipes atomic.Uint64

// Duplex overlapped named pipe for a server's stdin, so a read on our end tells of the server closing its own
func stdinPipe() (child, ours *os.File, err error) {
	name := fmt.Sprintf(`\\.\pipe\icebath-lsp-%d-%d`, os.Getpid(), pipes.Add(1))
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, nil, err
	}
	server, err := windows.CreateNamedPipe(name16, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT, 1, 64<<10, 64<<10, 0, nil)
	if err != nil {
		return nil, nil, err
	}
	client, err := windows.CreateFile(name16, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		windows.CloseHandle(server)
		return nil, nil, err
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(client)
		windows.CloseHandle(server)
		return nil, nil, err
	}
	defer windows.CloseHandle(event)
	overlapped := windows.Overlapped{HEvent: event}
	switch err := windows.ConnectNamedPipe(server, &overlapped); err {
	case nil, windows.ERROR_PIPE_CONNECTED:
	case windows.ERROR_IO_PENDING:
		var n uint32
		if err := windows.GetOverlappedResult(server, &overlapped, &n, true); err != nil {
			windows.CloseHandle(client)
			windows.CloseHandle(server)
			return nil, nil, err
		}
	default:
		windows.CloseHandle(client)
		windows.CloseHandle(server)
		return nil, nil, err
	}
	return os.NewFile(uintptr(client), name), os.NewFile(uintptr(server), name), nil
}

// Calls onClose once the server closes its end of the duplex stdin pipe, until the returned stop is called
func watchClose(f *os.File, onClose func()) (stop func()) {
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

// Windows has no groups to kill: the descendants are killed one by one
func killGroup(int) {}

// Whether the file is there, every file running on Windows
func executable(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
