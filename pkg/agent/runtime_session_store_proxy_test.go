package agent

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// A real HTTP proxy that records every request line and refuses it, one that cannot reach loopback
func refusingProxy(t *testing.T) (string, func() []string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var mu sync.Mutex
	var seen []string
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			mu.Lock()
			seen = append(seen, strings.TrimRight(line, "\r\n"))
			mu.Unlock()
			_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
			conn.Close()
		}
	}()
	return "http://" + listener.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestWorkerRpcReachesLoopbackOwnerWithProxyEnv(t *testing.T) {
	home := t.TempDir()
	startLoopbackOwner(t, home, "method", 0)
	proxy, seen := refusingProxy(t)
	for _, name := range []string{"NO_PROXY", "no_proxy", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("HTTPS_PROXY", proxy)
	rpc := NewWorkerRPC(home)
	defer rpc.Close()
	result, err := rpc.Call(context.Background(), "worker.persist", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(result, pyjson.ObjectOf("method", "worker.persist")) {
		t.Fatalf("result %v", result)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("proxy saw %v", got)
	}
}
