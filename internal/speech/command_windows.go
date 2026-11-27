//go:build windows

package speech

import "syscall"

// Command line handed to the shell as written, which cmd parses itself
func commandLine(line string) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CmdLine: line}
}
