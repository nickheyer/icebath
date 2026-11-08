//go:build windows

package kanban

import (
	"syscall"

	"golang.org/x/sys/windows"
)

const sigkillSignal = syscall.SIGTERM

// Terminates pid with exit code sig as os.kill does on Windows
func osKill(pid int, sig syscall.Signal) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, uint32(sig))
}
