//go:build !windows

package provider

import "syscall"

// Marks the loopback callback socket reusable so a fresh login can rebind a port in TIME_WAIT
func reuseAddrControl(network, address string, c syscall.RawConn) error {
	var opErr error
	if err := c.Control(func(fd uintptr) {
		opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return opErr
}
