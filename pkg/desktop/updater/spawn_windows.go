//go:build windows

package updater

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Starts the helper with its console window hidden
func detachHelper(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// Terminates the helper process
func killHelper(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// Windows reports no terminating signal
func exitSignalName(*os.ProcessState) string { return "" }

// Hides the updater console, detached into its own process group when asked
func detachUpdater(cmd *exec.Cmd, detached bool) {
	attr := &syscall.SysProcAttr{HideWindow: true}
	if detached {
		attr.CreationFlags = windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP
	}
	cmd.SysProcAttr = attr
}

// Runs a child with its console window hidden
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// Stops the PowerShell children of the cmd wrapper that name this desktop, and the wrapper while it runs
func killHandoffTree(child *UpdaterChild, _ bool, desktopPID int) {
	pid := child.PID()
	if pid <= 0 {
		return
	}
	command := fmt.Sprintf("Get-CimInstance Win32_Process -Filter 'ParentProcessId=%d' | Where-Object { $_.CommandLine -match '-DesktopPid\\s+%d(\\s|$)' } | ForEach-Object { taskkill.exe /PID $_.ProcessId /T /F | Out-Null }", pid, desktopPID)
	select {
	case <-child.done:
	default:
		command += fmt.Sprintf("; taskkill.exe /PID %d /T /F | Out-Null", pid)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command)
	hideConsole(cmd)
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}

// Windows version the minimum system version of a feed is compared with, major minor build as os.release reports it
func kernelRelease() string {
	major, minor, build := windows.RtlGetNtVersionNumbers()
	return fmt.Sprintf("%d.%d.%d", major, minor, build)
}
