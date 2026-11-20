//go:build !windows

package gateway

import "syscall"

// Address family error a wildcard bind skips like asyncio skips a family it cannot open
var errAddressFamilyUnsupported error = syscall.EAFNOSUPPORT

// Socket control applying an explicit asyncio reuse_address choice before bind
func reuseAddressControl(reuse *bool) func(network, address string, c syscall.RawConn) error {
	if reuse == nil {
		return nil
	}
	value := 0
	if *reuse {
		value = 1
	}
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, value)
		}); err != nil {
			return err
		}
		return serr
	}
}
