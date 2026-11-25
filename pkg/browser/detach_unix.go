//go:build unix

package browser

import "syscall"

// Process attributes of a browser started in its own session, out of reach of this terminal's signals
func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
