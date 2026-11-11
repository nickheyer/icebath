//go:build !windows

package selfupdate

import "syscall"

// New session for a recovery child so the gateway restart it runs never shares the updater group
func recoveryDetachedAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
