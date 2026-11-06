//go:build !windows

package home

import "syscall"

// New session so the prune outlives the launching command and its terminal
func detachAttr(bool) *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
