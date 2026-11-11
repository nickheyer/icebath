//go:build unix

package gatewayrun

import (
	"net"
	"net/netip"
	"os"
	"syscall"
)

// A fixed port binds again while the last gateway's connections sit in TIME_WAIT
const reuseFixedPort = true

// Listener socket bound before it listens, the socket.socket start_gateway_api hands the server
type listenerSocket struct {
	fd int
	ln net.Listener
}

// Unbound socket of addr's family, a fixed port rebindable at once and a dual stack one taking IPv4 too
func openListenerSocket(addr netip.Addr, port int, dualStack bool) (*listenerSocket, error) {
	family := syscall.AF_INET
	if !addr.Is4() {
		family = syscall.AF_INET6
	}
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(family, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, os.NewSyscallError("socket", err)
	}
	s := &listenerSocket{fd: fd}
	if dualStack {
		if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0); err != nil {
			s.close()
			return nil, os.NewSyscallError("setsockopt", err)
		}
	}
	if port != 0 && reuseFixedPort {
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
			s.close()
			return nil, os.NewSyscallError("setsockopt", err)
		}
	}
	return s, nil
}

// Binds the socket to addr and port
func (s *listenerSocket) bind(addr netip.Addr, port int) error {
	var sa syscall.Sockaddr = &syscall.SockaddrInet6{Port: port, Addr: addr.As16()}
	if addr.Is4() {
		sa = &syscall.SockaddrInet4{Port: port, Addr: addr.As4()}
	}
	return os.NewSyscallError("bind", syscall.Bind(s.fd, sa))
}

// Starts listening with backlog and hands the socket to a net listener, once
func (s *listenerSocket) listen(backlog int) (net.Listener, error) {
	if s.ln != nil {
		return s.ln, nil
	}
	if err := syscall.Listen(s.fd, backlog); err != nil {
		return nil, os.NewSyscallError("listen", err)
	}
	f := os.NewFile(uintptr(s.fd), "gateway-api")
	ln, err := net.FileListener(f)
	f.Close()
	s.fd = -1
	if err != nil {
		return nil, err
	}
	s.ln = ln
	return ln, nil
}

// Address and port the socket bound
func (s *listenerSocket) sockname() (string, int) {
	if s.ln != nil {
		a := s.ln.Addr().(*net.TCPAddr)
		return a.IP.String(), a.Port
	}
	sa, err := syscall.Getsockname(s.fd)
	if err != nil {
		return "", 0
	}
	switch a := sa.(type) {
	case *syscall.SockaddrInet4:
		return netip.AddrFrom4(a.Addr).String(), a.Port
	case *syscall.SockaddrInet6:
		return netip.AddrFrom16(a.Addr).String(), a.Port
	}
	return "", 0
}

// Closes the socket whether or not it listens yet
func (s *listenerSocket) close() {
	if s.ln != nil {
		s.ln.Close()
		return
	}
	if s.fd >= 0 {
		syscall.Close(s.fd)
		s.fd = -1
	}
}
