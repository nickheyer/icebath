//go:build unix

package browserutil

import "syscall"

// Process attributes of a browser started in its own session, out of reach of this terminal's signals
func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
