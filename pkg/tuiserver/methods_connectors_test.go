package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

const connectorRPCSID = "op-rpc-session"

// Frames of a recorder decoded into plain values
func recordedFrames(r *frameRecorder) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, 0, len(r.frames))
	for _, frame := range r.frames {
		out = append(out, pyjson.Plain(frame).(map[string]any))
	}
	return out
}

// Session RPC through the real dispatcher, a pool handler's reply read back from the client transport
func connectorRPCCall(t *testing.T, client *frameRecorder, method string, params map[string]any) map[string]any {
	t.Helper()
	merged := map[string]any{"owner": map[string]any{"type": "session", "session_id": connectorRPCSID}}
	for key, value := range params {
		merged[key] = value
	}
	before := client.count()
	response := Dispatch(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": merged}, client)
	if response != nil {
		return pyjson.Plain(response).(map[string]any)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		frames := recordedFrames(client)
		for i := len(frames) - 1; i >= before; i-- {
			if id, _ := frames[i]["id"].(int); id == 7 {
				return frames[i]
			}
			if id, _ := frames[i]["id"].(float64); id == 7 {
				return frames[i]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no reply to %s", method)
	return nil
}

// Event payloads of one kind the client received, waiting until at least expect arrived
func connectorEvents(client *frameRecorder, kind string, expect int) []map[string]any {
	collect := func() []map[string]any {
		var found []map[string]any
		for _, frame := range recordedFrames(client) {
			params, _ := frame["params"].(map[string]any)
			if frame["method"] == "event" && params["type"] == kind {
				found = append(found, params)
			}
		}
		return found
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if found := collect(); len(found) >= expect {
			return found
		}
		time.Sleep(10 * time.Millisecond)
	}
	return collect()
}

// Two pending MCP authorize targets opened as the session's live operation
func openConnectorOp(t *testing.T) *tools.ConnectionOperation {
	t.Helper()
	operation := tools.NewConnectionOperation([]*tools.ConnTarget{tools.NewConnTarget("gmail", "mcp", "authorize"),
		tools.NewConnTarget("notion", "mcp", "authorize")}, connectorRPCSID, "", nil)
	if err := tools.LiveOpen(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	return operation
}

// Transition that must be legal for the case to mean anything
func mustTransition(t *testing.T, operation *tools.ConnectionOperation, name string, to tools.TargetState, actor tools.ConnActor, fields tools.TransitionFields) {
	t.Helper()
	if _, err := operation.Transition(name, to, actor, fields); err != nil {
		t.Fatal(err)
	}
}

func strPtr(s string) *string { return &s }

func rpcCode(response map[string]any) int {
	failure, _ := response["error"].(map[string]any)
	switch code := failure["code"].(type) {
	case float64:
		return int(code)
	case int:
		return code
	case int64:
		return int(code)
	}
	return 0
}

// OAuth attempt of the fake MCP backend, pending until the case approves it
type fakeMCPAttempt struct {
	mu     sync.Mutex
	url    string
	status string
}

func (a *fakeMCPAttempt) AuthURL() string            { return a.url }
func (a *fakeMCPAttempt) Detail() string             { return "" }
func (a *fakeMCPAttempt) Flow() *mcp.ClientOAuthFlow { return nil }

// Approved attempts carry a discovery error so no server is dialed
func (a *fakeMCPAttempt) Poll() mcp.AttemptPoll {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status == "approved" {
		return mcp.AttemptPoll{Status: a.status, DiscoveryError: "tools are listed once the server is reachable"}
	}
	return mcp.AttemptPoll{Status: a.status}
}

// MCP backend whose OAuth starts are recorded by server name, a start failing with the reason set for its name
type fakeMCPBackend struct {
	mu       sync.Mutex
	base     string
	starts   []string
	failures map[string]string
	attempts map[string]*fakeMCPAttempt
}

func newFakeMCPBackend(base string) *fakeMCPBackend {
	return &fakeMCPBackend{base: base, failures: map[string]string{}, attempts: map[string]*fakeMCPAttempt{}}
}

func (b *fakeMCPBackend) RequiredEnv(context.Context, string) ([]*pyjson.Object, error) {
	return nil, nil
}
func (b *fakeMCPBackend) InstallsWithOAuth(context.Context, string) (bool, error) { return true, nil }
func (b *fakeMCPBackend) Install(context.Context, string, *pyjson.Object) ([]string, error) {
	return nil, errors.New("install is not driven by these cases")
}
func (b *fakeMCPBackend) Enable(context.Context, string) error { return nil }
func (b *fakeMCPBackend) StartInstallOAuth(ctx context.Context, name string, _ *pyjson.Object) (mcp.ConnectorAttempt, error) {
	return b.StartOAuth(ctx, name)
}

// Link numbered by the server's start count, or the failure set for the server
func (b *fakeMCPBackend) StartOAuth(_ context.Context, name string) (mcp.ConnectorAttempt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts = append(b.starts, name)
	if reason := b.failures[name]; reason != "" {
		return nil, errors.New(reason)
	}
	count := 0
	for _, started := range b.starts {
		if started == name {
			count++
		}
	}
	attempt := &fakeMCPAttempt{url: fmt.Sprintf("%s%s/%d", b.base, name, count), status: "pending"}
	b.attempts[name] = attempt
	return attempt, nil
}

// Server names of every OAuth start so far
func (b *fakeMCPBackend) started() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.starts)
}

// Sets or clears the failure of the next starts of one server
func (b *fakeMCPBackend) fail(name, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures[name] = reason
}

// Approves the server's open attempt
func (b *fakeMCPBackend) setConnected(name string) {
	b.mu.Lock()
	attempt := b.attempts[name]
	b.mu.Unlock()
	attempt.mu.Lock()
	attempt.status = "approved"
	attempt.mu.Unlock()
}

// Configures gmail and notion as MCP servers of the case home so authorize accepts them
func configureConnectorServers(t *testing.T) {
	t.Helper()
	body := "mcp_servers:\n  gmail:\n    url: https://gmail.example/mcp\n  notion:\n    url: https://notion.example/mcp\n"
	if err := os.WriteFile(filepath.Join(home.Process(), "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Card authorize of gmail and notion on the RPC session, returned once its card is drawn and settled at cleanup
func driveConnectorOp(t *testing.T, backend *fakeMCPBackend) *tools.ConnectionOperation {
	t.Helper()
	configureConnectorServers(t)
	drawn, finished := make(chan struct{}), make(chan struct{})
	ctx, env := setSessionContext(context.Background(), connectorRPCSID, nil, "")
	go func() {
		defer close(finished)
		defer clearSessionContext(env)
		tools.ManageConnections(ctx, env, map[string]any{"action": "authorize", "connectors": []any{"gmail", "notion"}},
			tools.ManageOptions{MCPBackend: backend, SessionID: connectorRPCSID, Callback: func(*pyjson.Object) string {
				close(drawn)
				return ""
			}})
	}()
	select {
	case <-drawn:
	case <-finished:
		t.Fatal("the operation ended before its card was drawn")
	case <-time.After(5 * time.Second):
		t.Fatal("no card drawn")
	}
	operation := tools.LiveCurrent(connectorRPCSID, "")
	t.Cleanup(func() {
		operation.Settle(tools.SettleContinue)
		operation.Wake.Set()
		<-finished
	})
	return operation
}

const connectorE2ESID = "connector-operation-e2e"

// Session RPC of the e2e session through the real dispatcher
func connectorE2ERPC(t *testing.T, client *frameRecorder, method string, params map[string]any) map[string]any {
	t.Helper()
	merged := map[string]any{"owner": map[string]any{"type": "session", "session_id": connectorE2ESID}}
	for key, value := range params {
		merged[key] = value
	}
	before := client.count()
	if response := Dispatch(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": merged}, client); response != nil {
		return pyjson.Plain(response).(map[string]any)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frames := recordedFrames(client)
		for i := len(frames) - 1; i >= before; i-- {
			if id, _ := frames[i]["id"].(int); id == 7 {
				return frames[i]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no reply to %s", method)
	return nil
}

// Event payloads of one kind matching a predicate, waiting up to five seconds for the first
func connectorE2EEvents(client *frameRecorder, kind string, match func(map[string]any) bool) []map[string]any {
	collect := func() []map[string]any {
		var found []map[string]any
		for _, frame := range recordedFrames(client) {
			params, _ := frame["params"].(map[string]any)
			if frame["method"] != "event" || params["type"] != kind {
				continue
			}
			payload, _ := params["payload"].(map[string]any)
			if payload == nil {
				payload = map[string]any{}
			}
			if match(payload) {
				found = append(found, payload)
			}
		}
		return found
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if found := collect(); len(found) > 0 {
			return found
		}
		time.Sleep(10 * time.Millisecond)
	}
	return collect()
}

func targetStates(targets any) map[string]any {
	out := map[string]any{}
	for _, item := range targets.([]any) {
		target := item.(map[string]any)
		out[target["name"].(string)] = target["state"]
	}
	return out
}
