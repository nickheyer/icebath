//go:build windows

package agent

import (
	"os"

	"golang.org/x/sys/windows"
)

// Private duplicate of the stdout handle as the frame channel, standard output then pointing at stderr
func managedWorkerStdio() (*os.File, error) {
	process := windows.CurrentProcess()
	var channel windows.Handle
	if err := windows.DuplicateHandle(process, windows.Handle(os.Stdout.Fd()), process, &channel, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	if err := windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(os.Stderr.Fd())); err != nil {
		_ = windows.CloseHandle(channel)
		return nil, err
	}
	os.Stdout = os.Stderr
	return os.NewFile(uintptr(channel), "managed-worker-channel"), nil
}
