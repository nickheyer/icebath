//go:build !windows

package tools

import "syscall"

// No console exists to hide outside Windows
func windowsHideAttr() *syscall.SysProcAttr { return nil }

// WindowsHideAttr is the console hiding attribute set, nil outside Windows
func WindowsHideAttr() *syscall.SysProcAttr { return windowsHideAttr() }

// Leaves caller attributes unchanged outside Windows
func mergeHideAttr(attr *syscall.SysProcAttr) *syscall.SysProcAttr { return attr }
