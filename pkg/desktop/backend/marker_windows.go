//go:build windows

package backend

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

const fileTimeToDotNetTicks = 504911232000000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

// IsPidAlive is true when the process can be opened or is refused for access
func IsPidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return true
	}
	return code == 259
}

// ProcessStartMarker returns win: and the creation time in .NET ticks, the value Get-Process StartTime reports
func ProcessStartMarker(pid int) (string, error) {
	if !IsPidAlive(pid) {
		return "", &NoProcessError{Pid: pid}
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", fmt.Errorf("Invalid Windows start marker for PID %d", pid)
	}
	ticks := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	return "win:" + strconv.FormatUint(ticks+fileTimeToDotNetTicks, 10), nil
}

// KillGroup is the POSIX group signal and never runs on Windows
func KillGroup(int, string) error { panic("process group signals run only on POSIX") }

// ForceKillProcessTree ends pid and its descendants with taskkill
func ForceKillProcessTree(pid int) {
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	hideWindow(cmd)
	cmd.Run()
}
