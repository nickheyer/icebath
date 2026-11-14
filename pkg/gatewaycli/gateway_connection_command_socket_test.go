package gatewaycli

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

type commandCall struct {
	method  string
	params  map[string]any
	timeout time.Duration
}

type commandClient struct {
	mu     sync.Mutex
	events *gateway.EventQueue
	calls  []commandCall
	closed bool
	hold   bool
	sent   *pythread.Event
}

func (c *commandClient) RPCTimeout(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	c.mu.Lock()
	c.calls = append(c.calls, commandCall{method, params, timeout})
	c.mu.Unlock()
	c.sent.Set()
	if c.hold {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return map[string]any{"type": "exec", "output": "done"}, nil
}

func (c *commandClient) EventQueue() *gateway.EventQueue { return c.events }
func (c *commandClient) SendFrame(string) error          { return nil }

func (c *commandClient) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *commandClient) state() ([]commandCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]commandCall{}, c.calls...), c.closed
}

type commandGateway struct {
	mu          sync.Mutex
	clients     []*commandClient
	hold        bool
	refuse      bool
	commandSent *pythread.Event
}

func (g *commandGateway) dial(context.Context) (ConnectionClient, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuse && len(g.clients) > 0 {
		return nil, &gateway.GatewayClientError{Msg: "Gateway connection failed; no local fallback"}
	}
	command := len(g.clients) > 0
	client := &commandClient{events: gateway.NewEventQueue(0), hold: g.hold && command, sent: &pythread.Event{}}
	if command {
		client.sent = g.commandSent
	}
	g.clients = append(g.clients, client)
	return client, nil
}

func (g *commandGateway) all() []*commandClient {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*commandClient{}, g.clients...)
}

func commandGoroutines() bool {
	buf := make([]byte, 1<<20)
	return strings.Contains(string(buf[:runtime.Stack(buf, true)]), "(*GatewayConnection).command(")
}

func commandsLeft(conn *GatewayConnection) int {
	conn.sideLock.Lock()
	defer conn.sideLock.Unlock()
	return len(conn.commands)
}

func TestTheReplyComesOverASocketOfItsOwnThatThenCloses(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	gw := &commandGateway{commandSent: &pythread.Event{}}
	conn := NewGatewayConnection(gw.dial)
	defer conn.Close(ctx)
	reply, err := conn.CallUntil(ctx, "slash.exec", &pythread.Event{}, map[string]any{"session_id": "s", "command": "quick"})
	if err != nil || !reflect.DeepEqual(reply, map[string]any{"type": "exec", "output": "done"}) {
		t.Fatal(reply, err)
	}
	clients := gw.all()
	if len(clients) != 2 {
		t.Fatalf("sockets %d", len(clients))
	}
	viewerCalls, viewerClosed := clients[0].state()
	if len(viewerCalls) != 0 || viewerClosed {
		t.Fatalf("viewer calls %v closed %v", viewerCalls, viewerClosed)
	}
	commandCalls, commandClosed := clients[1].state()
	want := []commandCall{{"slash.exec", map[string]any{"session_id": "s", "command": "quick"}, -1}}
	if !reflect.DeepEqual(commandCalls, want) || !commandClosed {
		t.Fatalf("command calls %v closed %v", commandCalls, commandClosed)
	}
	if commandGoroutines() || commandsLeft(conn) != 0 {
		t.Fatal("the command socket outlived its reply")
	}
}

func TestAStoppedWaitClosesTheCommandSocketAndNeverSendsAgain(t *testing.T) {
	for _, stop := range []string{"ctrl_c", "close"} {
		t.Run(stop, func(t *testing.T) {
			ctx := context.Background()
			t.Setenv("ICEBATH_HOME", t.TempDir())
			gw := &commandGateway{hold: true, commandSent: &pythread.Event{}}
			conn := NewGatewayConnection(gw.dial)
			defer conn.Close(ctx)
			if err := conn.Start(ctx, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			wake := &pythread.Event{}
			done := make(chan error, 1)
			go func() {
				_, err := conn.CallUntil(ctx, "slash.exec", wake, map[string]any{"session_id": "s", "command": "slow"})
				done <- err
			}()
			if !gw.commandSent.Wait(10) {
				t.Fatal("the command was never sent")
			}
			if stop == "ctrl_c" {
				wake.Set()
			} else {
				conn.Close(ctx)
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the stopped wait kept waiting")
			}
			var stopped *CallStopped
			if !errors.As(err, &stopped) {
				t.Fatalf("outcome %v", err)
			}
			clients := gw.all()
			viewerCalls, _ := clients[0].state()
			commandCalls, commandClosed := clients[1].state()
			if len(clients) != 2 || len(viewerCalls) != 0 || len(commandCalls) != 1 || !commandClosed {
				t.Fatalf("viewer %v command %v closed %v", viewerCalls, commandCalls, commandClosed)
			}
			if commandGoroutines() || commandsLeft(conn) != 0 {
				t.Fatal("the command socket outlived its wait")
			}
		})
	}
}

func TestAFailedCommandDialSendsNothingAndIsNotRetried(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	gw := &commandGateway{refuse: true, commandSent: &pythread.Event{}}
	conn := NewGatewayConnection(gw.dial)
	defer conn.Close(ctx)
	_, err := conn.CallUntil(ctx, "slash.exec", &pythread.Event{}, map[string]any{"session_id": "s", "command": "x"})
	wantClientError(t, err, "connection failed")
	var stopped *CallStopped
	if errors.As(err, &stopped) {
		t.Fatalf("a failed dial read as a stopped wait: %v", err)
	}
	clients := gw.all()
	if len(clients) != 1 {
		t.Fatalf("sockets %d", len(clients))
	}
	if calls, _ := clients[0].state(); len(calls) != 0 {
		t.Fatalf("viewer calls %v", calls)
	}
	if commandGoroutines() || commandsLeft(conn) != 0 {
		t.Fatal("the refused command left a socket behind")
	}
}
