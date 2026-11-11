//go:build windows

package kanban

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Process attributes starting the worker without a console window
func workerSpawnAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// Windows grants execution by extension, so every regular file found on PATH qualifies
func executableAccess(string) bool { return true }
