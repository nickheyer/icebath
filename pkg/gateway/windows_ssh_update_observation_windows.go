//go:build windows

package gateway

import (
	"errors"
	"strconv"

	"golang.org/x/sys/windows"
)

const observedStillActive = 259

// Whether a pid runs by its exit code, an invalid pid dead and any other failure unknown
func observedPIDAlive(pid int) (bool, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, true
		}
		return false, false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false, false
	}
	return code == observedStillActive, true
}

// windows FILETIME of the process creation, unknown when it cannot be read
func observedProcessCreation(pid int) (string, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", false
	}
	value := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	return "windows:" + strconv.FormatUint(value, 10), true
}
