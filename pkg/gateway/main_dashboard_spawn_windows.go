//go:build windows

package gateway

import "syscall"

// Process attributes of a respawned dashboard, Windows having no session to start
func dashboardDetachedAttr() *syscall.SysProcAttr { return nil }
