//go:build windows

package pm

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// No descriptors since Windows passes no checkout lock fds and the takeover leases the lock itself
func checkoutCustody(root string, lock UpdateLockHooks) ([]*os.File, error) { return nil, nil }

// Creation attributes starting the takeover suspended until the update job binds it
func suspendedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
}
