//go:build windows

package dashboard

import (
	"errors"
	"net"
	"syscall"
)

// Name of the asyncio transport class that tears a connection down on this platform
const transportClassName = "_ProactorSocketTransport"

// Shuts both directions of an open socket down then closes it, a peer that hung up raising its reset
func closeTransport(conn net.Conn) error {
	var shutdownErr error
	if sc, ok := conn.(syscall.Conn); ok {
		if raw, err := sc.SyscallConn(); err == nil {
			_ = raw.Control(func(fd uintptr) { shutdownErr = syscall.Shutdown(syscall.Handle(fd), syscall.SHUT_RDWR) })
		}
	}
	closeErr := conn.Close()
	if shutdownErr != nil {
		var errno syscall.Errno
		if errors.As(shutdownErr, &errno) {
			return errno
		}
		return shutdownErr
	}
	if closeErr == nil || errors.Is(closeErr, net.ErrClosed) || errors.Is(closeErr, syscall.WSAECONNRESET) {
		return nil
	}
	return closeErr
}
