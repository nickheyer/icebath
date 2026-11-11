//go:build !windows

package dashboard

import (
	"errors"
	"net"
	"syscall"
)

// Name of the asyncio transport class that tears a connection down on this platform
const transportClassName = "_SelectorSocketTransport"

// Closes the socket of a finished transport, a reset peer and an already closed socket raising nothing
func closeTransport(conn net.Conn) error {
	err := conn.Close()
	if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) {
		return nil
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	return err
}
