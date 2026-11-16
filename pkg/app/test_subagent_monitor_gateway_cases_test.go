package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
)

const subagentGatewaySID = "local-abc"

// Scripted gateway answering the session's subagent verbs
type subagentGatewayClient struct {
	mu          sync.Mutex
	events      *gateway.EventQueue
	calls       []scriptedCall
	children    []map[string]any
	steered     []any
	steerStatus string
	tailError   error
	listError   error
	tailText    string
}

func newSubagentGatewayClient() *subagentGatewayClient {
	return &subagentGatewayClient{events: gateway.NewEventQueue(0), steerStatus: "queued", tailText: "step 1\nstep 2\n",
		children: []map[string]any{{"subagent_id": "sa-0-1", "goal": "Audit the parser", "status": "running",
			"started_at": nowSeconds() - 7, "last_tool": "read_file", "accepting_steer": true}}}
}

func (c *subagentGatewayClient) RPCTimeout(_ context.Context, method string, params map[string]any, _ time.Duration) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, scriptedCall{method, params})
	switch method {
	case "subagent.list":
		if c.listError != nil {
			return nil, c.listError
		}
		children := make([]any, len(c.children))
		for i, child := range c.children {
			children[i] = copyMap(child)
		}
		return map[string]any{"subagents": children, "delegations": []any{}}, nil
	case "subagent.tail":
		if c.tailError != nil {
			return nil, c.tailError
		}
		return map[string]any{"subagent_id": params["subagent_id"], "available": true, "text": c.tailText, "truncated": false}, nil
	case "subagent.steer":
		c.steered = append(c.steered, params["text"])
		return map[string]any{"status": c.steerStatus, "subagent_id": params["subagent_id"], "text": params["text"]}, nil
	case "subagent.interrupt":
		found := false
		kept := c.children[:0:0]
		for _, child := range c.children {
			if child["subagent_id"] == params["subagent_id"] {
				found = true
				continue
			}
			kept = append(kept, child)
		}
		c.children = kept
		return map[string]any{"found": found, "subagent_id": params["subagent_id"]}, nil
	}
	return map[string]any{}, nil
}

func (c *subagentGatewayClient) EventQueue() *gateway.EventQueue { return c.events }
func (c *subagentGatewayClient) SendFrame(string) error          { return nil }
func (c *subagentGatewayClient) Close() error                    { return nil }

// Number of calls of the method so far
func (c *subagentGatewayClient) count(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, call := range c.calls {
		if call.method == method {
			n++
		}
	}
	return n
}

// Gateway agent of the scripted session bound as the process view, the local registry empty
func subagentGatewayFixture(t *testing.T) (*GatewayAgent, *subagentGatewayClient) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cleanSubagentRegistry(t)
	client := newSubagentGatewayClient()
	conn := gatewaycli.NewGatewayConnection(func(context.Context) (gatewaycli.ConnectionClient, error) { return client, nil })
	if err := conn.Start(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	conn.State(subagentGatewaySID)
	a, err := newGatewayAgent(context.Background(), gatewayAgentOptions{Connection: conn, SessionID: subagentGatewaySID, MaxIterations: 90,
		ToolProgressMode: "all", QuietMode: true})
	if err != nil {
		t.Fatal(err)
	}
	bindGatewayAgent(a)
	t.Cleanup(func() {
		conn.Close(context.Background())
		bindGatewayView(nil, nil)
	})
	return a, client
}
