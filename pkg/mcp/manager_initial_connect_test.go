package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Records every server task a connect builds, installing runner on each
func observeServerTasks(t *testing.T, runner func(s *serverTask) func(context.Context, *Rendered) (string, error)) func() []*serverTask {
	var mu sync.Mutex
	var created []*serverTask
	prev := newServerTaskFn
	newServerTaskFn = func(name string, env *tools.Env) *serverTask {
		s := newServerTask(name, env)
		s.transportRunner = runner(s)
		mu.Lock()
		created = append(created, s)
		mu.Unlock()
		return s
	}
	t.Cleanup(func() { newServerTaskFn = prev })
	return func() []*serverTask {
		mu.Lock()
		defer mu.Unlock()
		return append([]*serverTask(nil), created...)
	}
}

// Runner failing every transport attempt with err
func failingRunner(err error) func(*serverTask) func(context.Context, *Rendered) (string, error) {
	return func(*serverTask) func(context.Context, *Rendered) (string, error) {
		return func(context.Context, *Rendered) (string, error) { return "", err }
	}
}

// Asserts the parked task is registry owned with its connect error recorded
func assertRegistryOwnedPark(t *testing.T, name string, srv *serverTask, want string) {
	t.Helper()
	mcpMu.Lock()
	owned := mcpServers[ServerKey{Name: name}] == srv
	reason := serverConnectErrors[ServerKey{Name: name}]
	mcpMu.Unlock()
	if !owned || !strings.Contains(reason, want) {
		t.Fatalf("registry owns %v with error %q", owned, reason)
	}
	if !srv.runAlive() {
		t.Fatal("a recoverable initial failure was not parked")
	}
}

func TestInitialConnectFailureIsRegistryOwnedAndReaped(t *testing.T) {
	_, env := managerHome(t)
	maxInitialConnectRetries = 0
	logs := captureLogs(t)
	created := observeServerTasks(t, failingRunner(errors.New("deterministic initial failure")))
	ctx := context.Background()
	if names := registerMCPServers(ctx, env, map[string]*Rendered{"initial-failure": mockEntry(t, map[string]any{"connect_timeout": 5})}); len(names) != 0 {
		t.Fatalf("registered %v", names)
	}
	if len(created()) != 1 {
		t.Fatalf("created %d tasks", len(created()))
	}
	srv := created()[0]
	assertRegistryOwnedPark(t, "initial-failure", srv, "deterministic initial failure")
	ShutdownMCPServers(ctx, nil, nil, 5*time.Second)
	if logs.count("pending task(s) from the MCP loop") != 0 {
		t.Fatalf("shutdown left MCP tasks pending at loop stop: %s", logs.text())
	}
	if srv.runAlive() || runningLoop() != nil {
		t.Fatal("the parked task and the loop must be reaped by shutdown")
	}
}

func TestInitialConnectFailureRevivesSameRegisteredServer(t *testing.T) {
	_, env := managerHome(t)
	maxInitialConnectRetries = 0
	var backendUp, revivedOnce atomic.Bool
	var transportCalls atomic.Int64
	revived := make(chan struct{})
	created := observeServerTasks(t, func(s *serverTask) func(context.Context, *Rendered) (string, error) {
		return func(ctx context.Context, r *Rendered) (string, error) {
			transportCalls.Add(1)
			if !backendUp.Load() {
				return "", errors.New("backend still booting")
			}
			if revivedOnce.CompareAndSwap(false, true) {
				go func() {
					select {
					case <-s.ready.Wait():
						close(revived)
					case <-ctx.Done():
					}
				}()
			}
			return s.runStdio(ctx, r)
		}
	})
	ctx := context.Background()
	cfg := map[string]*Rendered{"recovering": mockEntry(t, map[string]any{"connect_timeout": 5,
		"env": map[string]any{"MOCK_MCP_TOOLS": "ping", "OWNED_MARKER": "revived:ok"}})}
	if names := registerMCPServers(ctx, env, cfg); len(names) != 0 || len(created()) != 1 {
		t.Fatalf("registered %v with %d tasks", names, len(created()))
	}
	srv := created()[0]
	assertRegistryOwnedPark(t, "recovering", srv, "backend still booting")
	backendUp.Store(true)
	registerMCPServers(ctx, env, cfg)
	select {
	case <-revived:
	case <-time.After(5 * time.Second):
		t.Fatal("cached parked server did not revive")
	}
	deadline := time.Now().Add(5 * time.Second)
	for tools.Default.Lookup(ctx, "", "mcp__recovering__ping") == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(created()) != 1 {
		t.Fatal("revival created a duplicate server task")
	}
	mcpMu.Lock()
	owned := mcpServers[ServerKey{Name: "recovering"}] == srv
	_, errored := serverConnectErrors[ServerKey{Name: "recovering"}]
	mcpMu.Unlock()
	srv.mu.Lock()
	parkErr := srv.err
	srv.mu.Unlock()
	if !owned || errored || transportCalls.Load() != 2 || srv.currentSession() == nil || parkErr != nil {
		t.Fatalf("owned %v errored %v calls %d err %v", owned, errored, transportCalls.Load(), parkErr)
	}
	entry := tools.Default.Lookup(ctx, "", "mcp__recovering__ping")
	if entry == nil || (entry.Check != nil && !entry.Check(env)) {
		t.Fatal("the revived tool is not registered and available")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(entry.Handler(ctx, &tools.Call{Name: entry.Name, Args: map[string]any{"value": "ok"}, Env: env})), &out); err != nil || out["result"] != "revived:ok" {
		t.Fatalf("dispatch %v %v", out, err)
	}
}

func TestInitialAuthFailureIsRetainedAndReaped(t *testing.T) {
	_, env := managerHome(t)
	created := observeServerTasks(t, failingRunner(&OAuthTokenError{Msg: "terminal authentication failure"}))
	ctx := context.Background()
	if names := registerMCPServers(ctx, env, map[string]*Rendered{"auth-failure": mockEntry(t, map[string]any{"connect_timeout": 5})}); len(names) != 0 || len(created()) != 1 {
		t.Fatalf("registered %v with %d tasks", names, len(created()))
	}
	srv := created()[0]
	assertRegistryOwnedPark(t, "auth-failure", srv, "terminal authentication failure")
	ShutdownMCPServers(ctx, nil, nil, 5*time.Second)
	if srv.runAlive() {
		t.Fatal("the retained auth failure must reap on shutdown")
	}
}

func TestStandaloneFailedConnectIsReapedWithoutGlobalOwner(t *testing.T) {
	_, env := managerHome(t)
	maxInitialConnectRetries = 0
	created := observeServerTasks(t, failingRunner(errors.New("probe target unavailable")))
	ctx := context.Background()
	_, err := runOnLoop(ctx, 5*time.Second, func(c context.Context) (*serverTask, error) {
		return connectServer(c, env, "probe-only", mockEntry(t, nil), nil)
	})
	if err == nil || !strings.Contains(err.Error(), "probe target unavailable") {
		t.Fatalf("probe error %v", err)
	}
	if len(created()) != 1 || created()[0].runAlive() {
		t.Fatal("a probe only failure must reap its task")
	}
	mcpMu.Lock()
	_, published := mcpServers[ServerKey{Name: "probe-only"}]
	_, errored := serverConnectErrors[ServerKey{Name: "probe-only"}]
	mcpMu.Unlock()
	if published || errored {
		t.Fatal("a probe only failure must not publish a parked server")
	}
}
