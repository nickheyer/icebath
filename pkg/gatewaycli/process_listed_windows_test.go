//go:build windows

package gatewaycli

import "golang.org/x/sys/windows"

// Reports a process object still open under the pid, an exited one whose handles remain included
func processListed(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}
