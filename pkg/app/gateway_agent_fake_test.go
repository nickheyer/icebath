package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Gateway client double answering every call through a handler and recording what was asked
type fakeGatewayClient struct {
	mu      sync.Mutex
	handler func(method string, params map[string]any) (any, error)
	calls   []string
	events  *gateway.EventQueue
	frames  []string
}

func (f *fakeGatewayClient) RPCTimeout(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	handler := f.handler
	f.mu.Unlock()
	if handler == nil {
		return pyjson.NewObject(), nil
	}
	return handler(method, params)
}

func (f *fakeGatewayClient) EventQueue() *gateway.EventQueue { return f.events }

func (f *fakeGatewayClient) SendFrame(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, text)
	return nil
}

func (f *fakeGatewayClient) Close() error { return nil }

// Methods the double was asked, in order
func (f *fakeGatewayClient) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Connection over a gateway double, closed when the case ends
func fakeGatewayConnection(t *testing.T, handler func(method string, params map[string]any) (any, error)) (*gatewaycli.GatewayConnection, *fakeGatewayClient) {
	t.Helper()
	client := &fakeGatewayClient{handler: handler, events: gateway.NewEventQueue(0)}
	connection := gatewaycli.NewGatewayConnection(func(context.Context) (gatewaycli.ConnectionClient, error) { return client, nil })
	t.Cleanup(func() { connection.Close(context.Background()) })
	return connection, client
}

// Gateway agent of the CLI session over a double that answers every call with an empty object
func offlineAgent(t *testing.T, c *CLI) *GatewayAgent {
	t.Helper()
	connection, _ := fakeGatewayConnection(t, nil)
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	a, err := newGatewayAgent(ctx, gatewayAgentOptions{Connection: connection, SessionID: c.sessionID, Model: "test/model", Provider: "openai",
		BaseURL: "http://127.0.0.1:9/v1", MaxIterations: 90, QuietMode: true, ToolProgressMode: "all"})
	if err != nil {
		t.Fatalf("newGatewayAgent: %v", err)
	}
	return a
}

// Gateway session store over a double whose reads go to the state.db of the case's home
func testGatewayStore(t *testing.T) *gatewaycli.GatewaySessionStore {
	t.Helper()
	connection, _ := fakeGatewayConnection(t, nil)
	store := gatewaycli.NewGatewaySessionStore(connection)
	t.Cleanup(func() { _ = store.CloseReader() })
	return store
}
