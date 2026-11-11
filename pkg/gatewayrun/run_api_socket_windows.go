//go:build windows

package gatewayrun

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"syscall"
)

// A fixed port is held for exclusive use so no other process binds it with SO_REUSEADDR
const exclusiveFixedPort = true

// Windows SO_EXCLUSIVEADDRUSE, the complement of SO_REUSEADDR
const soExclusiveAddrUse = ^syscall.SO_REUSEADDR

// Listener socket, its options taken now and applied by the bind that also listens
type listenerSocket struct {
	network string
	lc      net.ListenConfig
	ln      net.Listener
}

// Socket of addr's family, a fixed port held exclusively and a dual stack one taking IPv4 too
func openListenerSocket(addr netip.Addr, port int, dualStack bool) (*listenerSocket, error) {
	network := "tcp4"
	if !addr.Is4() {
		network = "tcp6"
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var optErr error
		err := c.Control(func(fd uintptr) {
			h := syscall.Handle(fd)
			if dualStack {
				if optErr = syscall.SetsockoptInt(h, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0); optErr != nil {
					return
				}
			}
			if port != 0 && exclusiveFixedPort {
				optErr = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, soExclusiveAddrUse, 1)
			}
		})
		if err != nil {
			return err
		}
		return optErr
	}}
	return &listenerSocket{network: network, lc: lc}, nil
}

// Binds the socket to addr and port, listening at once as Windows sockets do through the net package
func (s *listenerSocket) bind(addr netip.Addr, port int) error {
	ln, err := s.lc.Listen(context.Background(), s.network, net.JoinHostPort(addr.String(), strconv.Itoa(port)))
	if err != nil {
		return err
	}
	s.ln = ln
	return nil
}

// Net listener of the socket, already listening since Windows binds and listens in one call
func (s *listenerSocket) listen(int) (net.Listener, error) { return s.ln, nil }

// Address and port the socket bound
func (s *listenerSocket) sockname() (string, int) {
	a := s.ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

// Closes the socket once it is bound
func (s *listenerSocket) close() {
	if s.ln != nil {
		s.ln.Close()
	}
}
