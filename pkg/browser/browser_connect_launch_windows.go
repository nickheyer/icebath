//go:build windows

package browser

import (
	"syscall"
)

const detachedProcess = 0x00000008

// A detached process in a new process group so the user's debug browser outlives this process
func debugLaunchAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup}
}

// Sets SO_REUSEADDR on a probe socket before it binds
func reuseAddrControl(network, address string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}
