package gatewaycli

import (
	"context"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type fakeCall struct {
	method string
	params map[string]any
}

type fakeConnClient struct {
	mu     sync.Mutex
	events *gateway.EventQueue
	calls  []fakeCall
	known  map[string]bool
	frames []string
	rpc    func(ctx context.Context, method string, params map[string]any) (any, error)
}

func storeInfo() map[string]any {
	return map[string]any{"model": "fake-model", "cwd": "/w", "context_length": 128000, "enabled_toolsets": []any{"file"},
		"tools": map[string]any{"file": []any{"read_file", "write_file"}}, "skills": map[string]any{"devops": []any{"sdlc-review"}},
		"mcp_servers": []any{}, "tool_availability": map[string]any{"available_toolsets": []any{"file"},
			"unavailable_toolsets": []any{}, "lazy_tools": []any{}, "disabled_tools": []any{}}}
}

func storeSnapshot(sid string) map[string]any {
	return map[string]any{"session_id": sid, "stored_session_id": sid, "messages": []any{}, "revision": 3,
		"execution_generation": 0, "subscription_id": "sub-" + sid, "info": storeInfo()}
}

func newFakeConnClient(known ...string) *fakeConnClient {
	c := &fakeConnClient{events: gateway.NewEventQueue(0), known: map[string]bool{}}
	for _, sid := range known {
		c.known[sid] = true
	}
	c.rpc = c.base
	return c
}

func (c *fakeConnClient) record(method string, params map[string]any) {
	c.mu.Lock()
	c.calls = append(c.calls, fakeCall{method, params})
	c.mu.Unlock()
}

func (c *fakeConnClient) base(ctx context.Context, method string, params map[string]any) (any, error) {
	c.record(method, params)
	switch method {
	case "session.create":
		c.mu.Lock()
		c.known["local-new"] = true
		c.mu.Unlock()
		return storeSnapshot("local-new"), nil
	case "session.resume":
		sid, _ := params["session_id"].(string)
		c.mu.Lock()
		known := c.known[sid]
		c.mu.Unlock()
		if !known {
			return nil, &gateway.GatewayClientError{Msg: "not_found"}
		}
		return storeSnapshot(sid), nil
	}
	return map[string]any{"status": "ok", "detached": true}, nil
}

func (c *fakeConnClient) setRPC(rpc func(ctx context.Context, method string, params map[string]any) (any, error)) {
	c.mu.Lock()
	c.rpc = rpc
	c.mu.Unlock()
}

func (c *fakeConnClient) RPCTimeout(ctx context.Context, method string, params map[string]any, _ time.Duration) (any, error) {
	c.mu.Lock()
	rpc := c.rpc
	c.mu.Unlock()
	return rpc(ctx, method, params)
}

func (c *fakeConnClient) EventQueue() *gateway.EventQueue { return c.events }

func (c *fakeConnClient) SendFrame(text string) error {
	c.mu.Lock()
	c.frames = append(c.frames, text)
	c.mu.Unlock()
	return nil
}

func (c *fakeConnClient) Close() error { return nil }

func (c *fakeConnClient) all() []fakeCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fakeCall{}, c.calls...)
}

func (c *fakeConnClient) last() fakeCall {
	calls := c.all()
	return calls[len(calls)-1]
}

func (c *fakeConnClient) of(method string) []map[string]any {
	var out []map[string]any
	for _, call := range c.all() {
		if call.method == method {
			out = append(out, call.params)
		}
	}
	return out
}

func (c *fakeConnClient) lastOf(t *testing.T, method string) map[string]any {
	t.Helper()
	calls := c.of(method)
	if len(calls) == 0 {
		t.Fatalf("no %s call in %v", method, c.all())
	}
	return calls[len(calls)-1]
}

func unsetEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func storeFixture(t *testing.T) (*GatewaySessionStore, *fakeConnClient) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	unsetEnv(t, "ICEBATH_SESSION_SOURCE", "ICEBATH_SESSION_SOURCE_EXPLICIT", "ICEBATH_SINGLE_QUERY_SESSION", "TERMINAL_ENV",
		"TERMINAL_CWD", "ICEBATH_YOLO_MODE", "ICEBATH_IGNORE_RULES", "ICEBATH_SAFE_MODE", "ICEBATH_IGNORE_USER_CONFIG",
		"ICEBATH_ACCEPT_HOOKS")
	client := newFakeConnClient("local-old")
	conn := NewGatewayConnection(func(context.Context) (ConnectionClient, error) { return client, nil })
	if err := conn.Start(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return NewGatewaySessionStore(conn), client
}

func storeCLI(sessionID string, resumed bool) OpenSessionCLI {
	if sessionID == "" {
		sessionID = "20261012_000000_abcdef"
	}
	return OpenSessionCLI{SessionID: sessionID, Resumed: resumed}
}

func mustOpen(t *testing.T, db *GatewaySessionStore, cli OpenSessionCLI, launch map[string]any) string {
	t.Helper()
	sid, err := db.OpenSession(context.Background(), cli, launch)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

func wantEqual(t *testing.T, got, want any) {
	t.Helper()
	if !pyjson.Equal(got, want) && !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
