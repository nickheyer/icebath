//go:build windows

package gatewayrun

import (
	"net"
	"net/netip"
	"syscall"
	"testing"
)

// SO_REUSEADDR of a listener socket bound for port, the _reuse_address of the hermes cases
func reuseAddress(t *testing.T, port int) bool {
	t.Helper()
	loopback := netip.MustParseAddr("127.0.0.1")
	s, err := openListenerSocket(loopback, port, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.bind(loopback, port); err != nil {
		t.Fatal(err)
	}
	defer s.close()
	raw, err := s.ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var value int
	var optErr error
	if err := raw.Control(func(fd uintptr) {
		value, optErr = syscall.GetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR)
	}); err != nil || optErr != nil {
		t.Fatal(err, optErr)
	}
	return value != 0
}

func TestOnWindowsTheWebappsFixedPortNeverLetsAnotherProcessBindIt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if reuseAddress(t, freePort(t)) {
		t.Fatal("the fixed port socket carries SO_REUSEADDR")
	}
	if reuseAddress(t, 0) {
		t.Fatal("the private listener socket carries SO_REUSEADDR")
	}
}
