//go:build !windows

package tools

import "syscall"

// No console exists to hide outside Windows
func windowsHideAttr() *syscall.SysProcAttr { return nil }

// Leaves caller attributes unchanged outside Windows
func mergeHideAttr(attr *syscall.SysProcAttr) *syscall.SysProcAttr { return attr }
