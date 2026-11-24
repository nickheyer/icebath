//go:build windows

package fsutil

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Exit code GetExitCodeProcess reports for a process still running
const stillActive = 259

// Whether no process with the pid is running, one this process may not open still counting as alive
func processGone(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code != stillActive
}
