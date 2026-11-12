//go:build windows

package pm

import "golang.org/x/sys/windows"

// Reports pid as a process still running, access denied meaning another user owns a live one
func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	event, err := windows.WaitForSingleObject(h, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}
