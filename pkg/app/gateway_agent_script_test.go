package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const scriptSID = "local-abc"

// Whether two values are the same JSON document
func sameJSON(a, b any) bool {
	left, errA := pyjson.Dumps(pyjson.Plain(a), pyjson.SortKeys(true))
	right, errB := pyjson.Dumps(pyjson.Plain(b), pyjson.SortKeys(true))
	return errA == nil && errB == nil && left == right
}

// Event frame of the scripted session for generation seven, admission adm-1 unless another is named
func scriptFrame(kind string, payload map[string]any, admission ...string) map[string]any {
	params := map[string]any{"type": kind, "session_id": scriptSID, "execution_generation": 7, "payload": payload}
	adm := "adm-1"
	if len(admission) > 0 {
		adm = admission[0]
	}
	if adm != "" {
		params["admission_id"] = adm
	}
	return map[string]any{"jsonrpc": "2.0", "method": "event", "params": params}
}

type scriptedCall struct {
	method string
	params map[string]any
}

// Gateway double that answers calls and plays its script of frames on every prompt.submit
type scriptedClient struct {
	t             *testing.T
	mu            sync.Mutex
	events        *gateway.EventQueue
	calls         []scriptedCall
	script        []any
	result        map[string]any
	usage         map[string]any
	onInterrupt   map[string]any
	mutateReplies []any
	override      func(method string, params map[string]any) (any, bool, error)
	sent          []string
}

func (c *scriptedClient) record(method string, params map[string]any) {
	c.mu.Lock()
	c.calls = append(c.calls, scriptedCall{method, params})
	c.mu.Unlock()
}

func (c *scriptedClient) RPCTimeout(_ context.Context, method string, params map[string]any, _ time.Duration) (any, error) {
	c.mu.Lock()
	override := c.override
	c.mu.Unlock()
	if override != nil {
		if answer, handled, err := override(method, params); handled {
			if err != nil {
				return nil, err
			}
			return wireValue(c.t, answer), nil
		}
	}
	c.record(method, params)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch method {
	case "session.mutate":
		if len(c.mutateReplies) > 0 {
			reply := c.mutateReplies[0]
			c.mutateReplies = c.mutateReplies[1:]
			return wireValue(c.t, reply), nil
		}
	case "prompt.submit":
		script := append([]any(nil), c.script...)
		go c.play(script)
		return wireValue(c.t, map[string]any{"admission_id": "adm-1", "status": "queued", "execution_generation": 7}), nil
	case "prompt.receipt":
		result, usage := c.result, c.usage
		if result == nil {
			result = map[string]any{}
		}
		if usage == nil {
			usage = map[string]any{}
		}
		return wireValue(c.t, map[string]any{"admission_id": "adm-1", "status": "terminal", "result": result, "usage": usage}), nil
	case "session.resume":
		return wireValue(c.t, map[string]any{"session_id": params["session_id"], "revision": 1, "execution_generation": 7, "messages": []any{}}), nil
	case "session.interrupt":
		if c.onInterrupt != nil {
			c.events.PutNowait(wireValue(c.t, c.onInterrupt))
		}
		return wireValue(c.t, map[string]any{"execution_state": "running"}), nil
	}
	return wireValue(c.t, map[string]any{"status": "ok"}), nil
}

// Puts the script's frames on the socket, a disconnect ending it and a wait pausing it
func (c *scriptedClient) play(script []any) {
	for _, item := range script {
		switch v := item.(type) {
		case string:
			if v == "disconnect" {
				c.events.PutNowait(&gateway.GatewayClientError{Msg: "Gateway disconnected"})
				return
			}
			time.Sleep(300 * time.Millisecond)
		case func():
			v()
		default:
			c.events.PutNowait(wireValue(c.t, item))
		}
	}
}

func (c *scriptedClient) EventQueue() *gateway.EventQueue { return c.events }

func (c *scriptedClient) SendFrame(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, text)
	return nil
}

func (c *scriptedClient) Close() error { return nil }

// Every recorded call of one method, oldest first
func (c *scriptedClient) callsOf(method string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, call := range c.calls {
		if call.method == method {
			out = append(out, call.params)
		}
	}
	return out
}

// Parameters of the last recorded call of one method, failing the case when there is none
func (c *scriptedClient) lastCall(method string) map[string]any {
	c.t.Helper()
	calls := c.callsOf(method)
	if len(calls) == 0 {
		c.t.Fatalf("no %s call recorded", method)
	}
	return calls[len(calls)-1]
}

func (c *scriptedClient) clearCalls() {
	c.mu.Lock()
	c.calls = nil
	c.mu.Unlock()
}

func (c *scriptedClient) setScript(script ...any) {
	c.mu.Lock()
	c.script = script
	c.mu.Unlock()
}

// Response frames the viewer sent back for server requests, each as its id and result
func (c *scriptedClient) replies() [][2]any {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out [][2]any
	for _, raw := range c.sent {
		frame, err := pyjson.LoadsDict(raw)
		if err != nil {
			c.t.Fatalf("reply frame %q: %v", raw, err)
		}
		out = append(out, [2]any{frame.GetOr("id", nil), pyjson.Plain(frame.GetOr("result", nil))})
	}
	return out
}

// Started connection over a scripted gateway with the scripted session's state created
func scriptedConnection(t *testing.T, client *scriptedClient) *gatewaycli.GatewayConnection {
	t.Helper()
	client.t = t
	if client.events == nil {
		client.events = gateway.NewEventQueue(0)
	}
	conn := gatewaycli.NewGatewayConnection(func(context.Context) (gatewaycli.ConnectionClient, error) { return client, nil })
	if err := conn.Start(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("connection start: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	conn.State(scriptSID)
	return conn
}

// Gateway agent of the scripted session with the defaults the Python constructor has
func scriptedAgentOn(t *testing.T, conn *gatewaycli.GatewayConnection, o gatewayAgentOptions) *GatewayAgent {
	t.Helper()
	o.Connection, o.SessionID = conn, scriptSID
	if o.MaxIterations == 0 {
		o.MaxIterations = 90
	}
	if o.ToolProgressMode == "" {
		o.ToolProgressMode = "all"
	}
	o.QuietMode = true
	a, err := newGatewayAgent(context.Background(), o)
	if err != nil {
		t.Fatalf("newGatewayAgent: %v", err)
	}
	return a
}

// Agent and its scripted gateway for one case, the home isolated
func makeScriptedAgent(t *testing.T, client *scriptedClient, o gatewayAgentOptions) (*GatewayAgent, *scriptedClient) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	conn := scriptedConnection(t, client)
	return scriptedAgentOn(t, conn, o), client
}

// Runs one turn over the given history and fails the case on a raised error
func runTurn(t *testing.T, a *GatewayAgent, message any, history ...map[string]any) map[string]any {
	t.Helper()
	result, err := a.RunConversation(context.Background(), message, agent.RunConversationOptions{ConversationHistory: history})
	if err != nil {
		t.Fatalf("RunConversation: %v", err)
	}
	return result
}

// Decoded JSON text
func loadsJSON(text string) (any, error) { return pyjson.Loads(text) }
