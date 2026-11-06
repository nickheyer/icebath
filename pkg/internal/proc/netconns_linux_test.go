package proc

import (
	"net"
	"os"
	"slices"
	"testing"
)

func TestInetConnectionStatusesSeesListenAndEstablished(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	statuses, err := InetConnectionStatuses(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"LISTEN", "ESTABLISHED", "NONE"} {
		if !slices.Contains(statuses, want) {
			t.Fatalf("statuses %v lack %s", statuses, want)
		}
	}
	if _, err := InetConnectionStatuses(1 << 30); err == nil {
		t.Fatal("a missing process must fail like psutil NoSuchProcess")
	}
}
