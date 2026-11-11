//go:build unix

package gatewayrun

import (
	"net"
	"net/netip"
	"syscall"
	"testing"
)

// SO_REUSEADDR of a fresh listener socket for port, the _reuse_address of the hermes cases
func reuseAddress(t *testing.T, port int) bool {
	s, err := openListenerSocket(netip.MustParseAddr("127.0.0.1"), port, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	v, err := syscall.GetsockoptInt(s.fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR)
	if err != nil {
		t.Fatal(err)
	}
	return v != 0
}

func TestOnPosixTheWebappsFixedPortBindsAgainAtOnce(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if !reuseAddress(t, 9120) {
		t.Fatal("fixed port socket lacks SO_REUSEADDR")
	}
	if reuseAddress(t, 0) {
		t.Fatal("private listener socket carries SO_REUSEADDR")
	}
}

func TestADualStackListenerTakesIPv4Too(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, err := openListenerSocket(netip.IPv6Unspecified(), 9120, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	v, err := syscall.GetsockoptInt(s.fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY)
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Fatalf("IPV6_V6ONLY = %d", v)
	}
}

func TestAFixedPortBindsAgainWhileTheLastGatewaysConnectionsLinger(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	loopback := netip.MustParseAddr("127.0.0.1")
	first, err := openListenerSocket(loopback, port, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.bind(loopback, port); err != nil {
		t.Fatal(err)
	}
	ln, err := first.listen(128)
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	served, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	served.Close()
	client.Read(make([]byte, 1))
	client.Close()
	first.close()
	second, err := openListenerSocket(loopback, port, false)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	if err := second.bind(loopback, port); err != nil {
		t.Fatalf("rebind while TIME_WAIT lingers: %v", err)
	}
}
