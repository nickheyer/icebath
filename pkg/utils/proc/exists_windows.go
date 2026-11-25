//go:build windows

package proc

import (
	"math"

	"golang.org/x/sys/windows"
)

// Reports a live process for pid, access denied meaning another user owns a running one and a pid outside the 32-bit process id range naming no process
func Exists(pid int) bool {
	if pid <= 0 || int64(pid) > math.MaxUint32 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}
