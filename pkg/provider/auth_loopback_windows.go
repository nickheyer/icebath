//go:build windows

package provider

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Marks the loopback callback socket reusable so a fresh login can rebind a port in TIME_WAIT
func reuseAddrControl(network, address string, c syscall.RawConn) error {
	var opErr error
	if err := c.Control(func(fd uintptr) {
		opErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return opErr
}
