package gateway

import "syscall"

// Process attributes of a route script, hidden from the console on Windows
func webhookScriptProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x08000000}
}
