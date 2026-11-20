//go:build windows

package homeguard

import "syscall"

// Creation flag of a process with no console, so closing or interrupting the console of the test run leaves it alone
const detachedProcess = 0x00000008

// Process attributes of a sweeper with no console and a process group of its own
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess, HideWindow: true}
}
