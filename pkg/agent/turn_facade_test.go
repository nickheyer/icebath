package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestRelayOutcomeForFailure(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{plugins.UserInterrupt{}, "cancelled"},
		{fmt.Errorf("wrapped: %w", context.Canceled), "cancelled"},
		{context.DeadlineExceeded, "timed_out"},
		{&net.OpError{Op: "dial", Err: timeoutErr{}}, "timed_out"},
		{errors.New("boom"), "failed"},
		{fmt.Errorf("tool: %w", tools.ErrKeyboardInterrupt), "cancelled"},
		{"panic text", "failed"},
	}
	for _, c := range cases {
		if got := relayOutcomeForFailure(t.Context(), c.v); got != c.want {
			t.Errorf("%#v: got %s want %s", c.v, got, c.want)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

func TestCacheScopeAgentKeepsOneMemoInstance(t *testing.T) {
	a := &AIAgent{sessionID: "s1", platform: "cli", provider: "anthropic", model: "m"}
	first := a.cacheScopeAgent()
	a.sessionID = "s2"
	second := a.cacheScopeAgent()
	if first != second || second.SessionID != "s2" || second.Platform != "cli" || second.SessionDB != nil || second.CompressionCount() != 0 {
		t.Fatalf("scope %+v", second)
	}
}

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	return client.(*net.TCPConn), server.(*net.TCPConn)
}

func TestSocketIsDeadProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		client, server := tcpPair(t)
		defer client.Close()
		defer server.Close()
		if _, err := socketIsDead(client); err == nil {
			t.Fatal("windows probe must fail like the missing MSG_DONTWAIT")
		}
		return
	}
	client, server := tcpPair(t)
	defer client.Close()
	if dead, err := socketIsDead(client); err != nil || dead {
		t.Fatalf("idle healthy socket reported dead=%v err=%v", dead, err)
	}
	server.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		dead, err := socketIsDead(client)
		if err != nil {
			t.Fatal(err)
		}
		if dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("half closed socket never reported dead")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var _ syscall.Conn = client
}

type noClientsAgent struct{ replaced, closed int }

func (n *noClientsAgent) primaryClient() any                     { return nil }
func (n *noClientsAgent) cachedRequestClient() any               { return nil }
func (n *noClientsAgent) replacePrimaryOpenAIClient(string) bool { n.replaced++; return true }
func (n *noClientsAgent) closeCachedRequestOpenAIClient(string)  { n.closed++ }

func TestCleanupDeadConnectionsWithoutClients(t *testing.T) {
	n := &noClientsAgent{}
	if cleanupDeadConnections(n) || n.replaced+n.closed != 0 {
		t.Fatal("no clients means nothing to clean")
	}
	a := &AIAgent{requestClientCache: map[string]any{"client": "c"}}
	if a.cachedRequestClient() != "c" || (&AIAgent{}).cachedRequestClient() != nil {
		t.Fatal("request cache client")
	}
}
