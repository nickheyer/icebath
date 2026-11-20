package agent

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Client whose tracked pool holds one TCP socket, half closed by its peer when dead
func clientWithSocket(t *testing.T, dead bool) *http.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	server := <-accepted
	if dead {
		server.Close()
		time.Sleep(50 * time.Millisecond)
	} else {
		t.Cleanup(func() { server.Close() })
	}
	return &http.Client{Transport: provider.TrackPoolConns(&http.Transport{}, conn)}
}

// Agent surface double recording which cleanup hook ran with which reason
type deadConnAgent struct {
	primary, request any
	replaced, closed []string
}

func (d *deadConnAgent) primaryClient() any       { return d.primary }
func (d *deadConnAgent) cachedRequestClient() any { return d.request }
func (d *deadConnAgent) replacePrimaryOpenAIClient(reason string) bool {
	d.replaced = append(d.replaced, reason)
	return true
}
func (d *deadConnAgent) closeCachedRequestOpenAIClient(reason string) {
	d.closed = append(d.closed, reason)
}

func TestCleanupRetiresDeadCachedRequestClient(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &deadConnAgent{primary: clientWithSocket(t, false), request: clientWithSocket(t, true)}
	if !cleanupDeadConnections(a) {
		t.Fatal("no cleanup")
	}
	if len(a.replaced) != 0 || len(a.closed) != 1 || a.closed[0] != "dead_connection_cleanup" {
		t.Fatalf("replaced %v closed %v", a.replaced, a.closed)
	}
}

func TestCleanupRebuildsDeadPrimaryClient(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &deadConnAgent{primary: clientWithSocket(t, true), request: clientWithSocket(t, false)}
	if !cleanupDeadConnections(a) {
		t.Fatal("no cleanup")
	}
	if len(a.replaced) != 1 || a.replaced[0] != "dead_connection_cleanup" || len(a.closed) != 0 {
		t.Fatalf("replaced %v closed %v", a.replaced, a.closed)
	}
}
