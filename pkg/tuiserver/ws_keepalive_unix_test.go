//go:build unix

package tuiserver

import (
	"net"
	"syscall"
	"testing"
)

// Socket of the keepalive cases that carries only the raw stream connection
type wsRawSocket struct {
	*wsTestSocket
	conn net.Conn
}

func (s *wsRawSocket) RawConn() net.Conn { return s.conn }

func TestWsSocketEnablesKeepalive(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()
	disableNagle(&wsRawSocket{wsTestSocket: newWSTestSocket(), conn: server})
	raw, err := server.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var nodelay, keepalive int
	var optErr error
	if err := raw.Control(func(fd uintptr) {
		if nodelay, optErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY); optErr != nil {
			return
		}
		keepalive, optErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
	}); err != nil || optErr != nil {
		t.Fatal(err, optErr)
	}
	if nodelay == 0 {
		t.Fatal("Nagle is still enabled")
	}
	if keepalive == 0 {
		t.Fatal("keepalive is not enabled")
	}
}

func TestWsSocketUnreachableIsSilent(t *testing.T) {
	disableNagle(newWSTestSocket())
}
