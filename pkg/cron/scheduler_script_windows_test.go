//go:build windows

package cron

import (
	"os"

	"golang.org/x/sys/windows"
)

const processStillActive = 259

// Open and still running per its exit code
func scriptProcessLive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	return windows.GetExitCodeProcess(handle, &code) == nil && code == processStillActive
}

func killScriptProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		p.Kill()
	}
}
