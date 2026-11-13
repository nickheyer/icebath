package connections

import "golang.org/x/sys/windows"

const stillActive = 259

// Signal zero existence probe through a terminate capable handle, an unopenable process counting as absent
func signalZero(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var status uint32
	if windows.GetExitCodeProcess(h, &status) != nil || status != stillActive {
		return false
	}
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}
