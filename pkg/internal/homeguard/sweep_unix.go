//go:build !windows

package homeguard

import "syscall"

// Process attributes of a sweeper in a session of its own, out of reach of the terminal and of signals sent to the test run
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
