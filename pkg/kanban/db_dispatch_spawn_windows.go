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
