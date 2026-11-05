//go:build windows

package pm

import (
	"os"

	"golang.org/x/sys/windows"
)

// Takes the protocol pipes off the standard handles so every child sees stderr for stdout and EOF for stdin
func workerStdio() (controls, wire *os.File, err error) {
	process := windows.CurrentProcess()
	var wireHandle, controlsHandle windows.Handle
	if err := windows.DuplicateHandle(process, windows.Handle(os.Stdout.Fd()), process, &wireHandle, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, nil, err
	}
	if err := windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(os.Stderr.Fd())); err != nil {
		return nil, nil, err
	}
	if err := windows.DuplicateHandle(process, windows.Handle(os.Stdin.Fd()), process, &controlsHandle, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, nil, err
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, nil, err
	}
	if err := windows.SetStdHandle(windows.STD_INPUT_HANDLE, windows.Handle(null.Fd())); err != nil {
		return nil, nil, err
	}
	os.Stdout, os.Stdin = os.Stderr, null
	return os.NewFile(uintptr(controlsHandle), "pm-controls"), os.NewFile(uintptr(wireHandle), "pm-wire"), nil
}

// Stops a worker still running after the exchange with TerminateProcess
func terminateWorker(p *os.Process) {
	_ = p.Kill()
}
