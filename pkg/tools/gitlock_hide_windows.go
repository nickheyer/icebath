//go:build windows

package tools

import "syscall"

// CREATE_NO_WINDOW, hiding the child console without detaching it
const gitlockCreateNoWindow = 0x08000000

// Attributes hiding the child console as windows_hide_flags does
func windowsHideAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: gitlockCreateNoWindow}
}

// Copy of attr with the console hiding flag added to its creation flags
func mergeHideAttr(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	merged := *attr
	merged.CreationFlags |= gitlockCreateNoWindow
	return &merged
}
