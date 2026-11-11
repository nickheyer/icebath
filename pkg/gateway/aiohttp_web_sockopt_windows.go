package gateway

import "syscall"

// Address family error a wildcard bind skips like asyncio skips a family it cannot open
var errAddressFamilyUnsupported error = syscall.Errno(10047)

// Socket control setting SO_REUSEADDR when asyncio reuse_address is explicitly true
func reuseAddressControl(reuse *bool) func(network, address string, c syscall.RawConn) error {
	if reuse == nil || !*reuse {
		return nil
	}
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		}); err != nil {
			return err
		}
		return serr
	}
}
