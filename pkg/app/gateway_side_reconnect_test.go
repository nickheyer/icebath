package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const reconnectSID = "local-abc"

// Event frame of the reconnect session for admission adm-1
func reconnectFrame(kind string, payload map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{
		"type": kind, "session_id": reconnectSID, "admission_id": "adm-1", "execution_generation": 7, "payload": payload}}
}

// One gateway process as one client sees it, from a dial until it hangs up
type reconnectLife struct {
	number       int
	owner        *reconnectGateway
	events       *gateway.EventQueue
	dropOnSubmit bool
	mu           sync.Mutex
	calls        []scriptedCall
}

func (l *reconnectLife) RPCTimeout(_ context.Context, method string, params map[string]any, _ time.Duration) (any, error) {
	l.mu.Lock()
	l.calls = append(l.calls, scriptedCall{method, params})
	drop := l.dropOnSubmit
	l.mu.Unlock()
	switch method {
	case "session.resume":
		if err := l.owner.nextResumeError(); err != nil {
			return nil, err
		}
		return map[string]any{"stored_session_id": params["session_id"], "subscription_id": fmt.Sprintf("sub-%d", l.number),
			"revision": 1, "execution_generation": 7, "messages": []any{}}, nil
	case "prompt.submit":
		var event any = &gateway.GatewayClientError{Msg: "Gateway disconnected"}
		if !drop {
			event = reconnectFrame("message.complete", map[string]any{"text": fmt.Sprintf("reply %d", l.number), "outcome": "completed", "status": "complete"})
		}
		go l.events.PutNowait(event)
		return map[string]any{"admission_id": "adm-1", "status": "queued", "execution_generation": 7}, nil
	case "prompt.receipt":
		return map[string]any{"result": map[string]any{"final_response": fmt.Sprintf("reply %d", l.number), "completed": true}, "usage": map[string]any{}}, nil
	case "prompt.background", "prompt.btw":
		return map[string]any{"task_id": fmt.Sprintf("task-%d", l.number)}, nil
	}
	return map[string]any{"status": "ok", "life": l.number}, nil
}

func (l *reconnectLife) EventQueue() *gateway.EventQueue { return l.events }
func (l *reconnectLife) SendFrame(string) error          { return nil }
func (l *reconnectLife) Close() error                    { return nil }

// Methods this life was called with, in order
func (l *reconnectLife) methods() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.calls))
	for _, call := range l.calls {
		out = append(out, call.method)
	}
	return out
}

// Each dial reaches a fresh gateway life, refusals make the next dials fail first
type reconnectGateway struct {
	mu             sync.Mutex
	lives          []*reconnectLife
	refusals       int
	dropNextSubmit bool
	resumeErrors   []error
}

func (g *reconnectGateway) nextResumeError() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.resumeErrors) == 0 {
		return nil
	}
	err := g.resumeErrors[0]
	g.resumeErrors = g.resumeErrors[1:]
	return err
}

func (g *reconnectGateway) dial(context.Context) (gatewaycli.ConnectionClient, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refusals > 0 {
		g.refusals--
		return nil, &gateway.GatewayClientError{Msg: "Gateway starting: deadline. Your message was not sent."}
	}
	life := &reconnectLife{number: len(g.lives) + 1, owner: g, events: gateway.NewEventQueue(0), dropOnSubmit: g.dropNextSubmit}
	g.dropNextSubmit = false
	g.lives = append(g.lives, life)
	return life, nil
}

func (g *reconnectGateway) life(index int) *reconnectLife {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lives[index]
}

func (g *reconnectGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.lives)
}

// The current life goes away and the connection's loop ends with it
func (g *reconnectGateway) hangUp(t *testing.T, conn *gatewaycli.GatewayConnection) {
	t.Helper()
	g.life(g.count() - 1).events.PutNowait(&gateway.GatewayClientError{Msg: "Gateway disconnected"})
	deadline := time.Now().Add(5 * time.Second)
	for conn.Client() != nil || !conn.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("the loop outlived its gateway")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Started connection on the fake gateway with the session's snapshot adopted
func reconnectConnection(t *testing.T, connect gatewaycli.ConnectFunc, conn *gatewaycli.GatewayConnection) *gatewaycli.GatewayConnection {
	t.Helper()
	ctx := context.Background()
	if conn == nil {
		conn = gatewaycli.NewGatewayConnection(connect)
	}
	if err := conn.Start(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	snapshot, err := conn.Call(ctx, "session.resume", map[string]any{"session_id": reconnectSID})
	if err != nil {
		t.Fatal(err)
	}
	conn.State(reconnectSID).AdoptSnapshot(snapshot)
	return conn
}

func reconnectFixture(t *testing.T) (*reconnectGateway, *gatewaycli.GatewayConnection) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	gw := &reconnectGateway{}
	return gw, reconnectConnection(t, gw.dial, nil)
}

func TestATurnLostToARestartIsReportedPlainlyAndTheNextTurnRuns(t *testing.T) {
	gw, conn := reconnectFixture(t)
	a := scriptedAgentOn(t, conn, gatewayAgentOptions{})
	first := gw.life(0)
	first.mu.Lock()
	first.dropOnSubmit = true
	first.mu.Unlock()
	lost, err := a.RunConversation(context.Background(), "first", agent.RunConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	message := pyrepr.Str(lost["error"])
	if lost["failed"] != true || lost["failure_reason"] != "gateway_connection" {
		t.Fatalf("lost %v", lost)
	}
	if !strings.Contains(message, "turn outcome is unknown") && !strings.Contains(message, "Gateway disconnected") {
		t.Fatalf("error %q", message)
	}
	if shown := chatErrorResponse(context.Background(), message, "", "", "gateway_connection"); shown != message {
		t.Fatalf("panel %q", shown)
	}
	deadline := time.Now().Add(5 * time.Second)
	for (conn.Client() != nil || !conn.Closed()) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	result, err := a.RunConversation(context.Background(), "second", agent.RunConversationOptions{})
	if err != nil || result["failed"] == true || result["final_response"] != "reply 2" {
		t.Fatalf("second %v %v", result, err)
	}
	if methods := gw.life(1).methods(); len(methods) < 2 || methods[0] != "session.resume" || methods[1] != "prompt.submit" {
		t.Fatalf("calls %v", methods)
	}
}

func TestThePreviousConnectionsLossNeverFailsALiveTurn(t *testing.T) {
	gw, conn := reconnectFixture(t)
	a := scriptedAgentOn(t, conn, gatewayAgentOptions{})
	gw.hangUp(t, conn)
	result, err := a.RunConversation(context.Background(), "hi", agent.RunConversationOptions{})
	if err != nil || result["failed"] == true || result["final_response"] != "reply 2" {
		t.Fatalf("result %v %v", result, err)
	}
}
