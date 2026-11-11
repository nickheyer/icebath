//go:build windows

package app

import "syscall"

// Whether the process attributes ask for a detached child
func detachRequested(a *syscall.SysProcAttr) bool { return a != nil && a.CreationFlags != 0 }
