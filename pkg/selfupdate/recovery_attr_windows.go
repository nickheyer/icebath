//go:build windows

package selfupdate

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// New process group with no console for a recovery child so it never shares the updater group
func recoveryDetachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}
