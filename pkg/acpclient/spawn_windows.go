//go:build windows

package acpclient

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Process attributes hiding the console window the CLI would flash, stdio pipes left intact
func hideWindowAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
