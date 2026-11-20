//go:build !windows

package gateway

import "syscall"

// Probe attributes of a service manager command, none outside Windows
func hiddenServiceProbeAttr() *syscall.SysProcAttr { return nil }
