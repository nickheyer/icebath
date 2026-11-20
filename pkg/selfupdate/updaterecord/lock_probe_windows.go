//go:build windows

package updaterecord

import (
	"errors"

	"golang.org/x/sys/windows"
)

const filetimeUnixEpoch = 116444736000000000

// Liveness through a SYNCHRONIZE handle, an access denied open counting as live
func pidRunning(pid int) (bool, error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED), nil
	}
	defer windows.CloseHandle(handle)
	event, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT), nil
}

// GetProcessTimes creation FILETIME in 100 ns ticks since 1601, false when unreadable
func windowsCreateFiletime(pid int) (uint64, bool) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(handle)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	return uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), true
}

// Creation time in unix seconds from the creation FILETIME, nil when unreadable
func stdlibCreateTime(pid int) *float64 {
	ticks, ok := windowsCreateFiletime(pid)
	if !ok {
		return nil
	}
	value := float64(int64(ticks)-filetimeUnixEpoch) / 1e7
	return &value
}
