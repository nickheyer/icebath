//go:build !windows

package acpclient

import "syscall"

// No process attributes outside Windows where no console window exists to hide
func hideWindowAttr() *syscall.SysProcAttr { return nil }
