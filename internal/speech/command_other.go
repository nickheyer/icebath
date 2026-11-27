//go:build !windows

package speech

import "syscall"

// Only Windows takes a raw command line
func commandLine(string) *syscall.SysProcAttr { return nil }
