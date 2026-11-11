//go:build !windows

package gateway

import "syscall"

// Process attributes starting a respawned dashboard in its own session
func dashboardDetachedAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
