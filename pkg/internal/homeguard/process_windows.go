//go:build windows

package homeguard

import "syscall"

// Query right that opens a process of any integrity level for its exit code
const processQueryLimitedInformation = 0x1000

// Exit code GetExitCodeProcess reports for a process still running
const stillActive = 259

// Whether pid names a live process, a process this user cannot open counting as live
func alive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return err == syscall.ERROR_ACCESS_DENIED
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

// Windows has no pid namespaces, every process shares namespace zero
func pidNamespace() string { return "0" }
