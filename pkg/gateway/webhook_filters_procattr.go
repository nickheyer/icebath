//go:build !windows

package gateway

import "syscall"

// Process attributes of a route script, none outside Windows
func webhookScriptProcAttr() *syscall.SysProcAttr { return nil }
