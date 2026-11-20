//go:build !windows

package browser

import (
	"syscall"
)

// A new session so the user's debug browser outlives this process
func debugLaunchAttrs() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// Sets SO_REUSEADDR on a probe socket before it binds
func reuseAddrControl(network, address string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}
