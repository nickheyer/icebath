//go:build !windows

package selfupdate

import "syscall"

// New session for the replayed gateway so it outlives the updater
func posixPauseDetachedAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
