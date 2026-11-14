package remote

import (
	"errors"
	"net"
	"testing"
)

func TestRefusedAndResetConnectionsCarryTheErrnoText(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := ln.Addr().String()
	ln.Close()
	var te *TransportError
	err = requestError(t, "http://"+gone)
	if want := "GET http://" + gone + "/v1/config/self?profile=p: [Errno 111] Connection refused"; !errors.As(err, &te) || te.Message != want {
		t.Fatalf("refused: %v", err)
	}
	reset := rawServer(t, func(conn net.Conn) {
		buf := make([]byte, 65536)
		conn.Read(buf)
		conn.(*net.TCPConn).SetLinger(0)
		conn.Close()
	})
	err = requestError(t, "http://"+reset)
	if want := "GET http://" + reset + "/v1/config/self?profile=p: [Errno 104] Connection reset by peer"; !errors.As(err, &te) || te.Message != want {
		t.Fatalf("reset: %v", err)
	}
}
