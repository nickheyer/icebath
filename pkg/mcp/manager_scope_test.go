package mcp

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMCPStatusScopedShutdownClearsOnlyItsConnectionStatus(t *testing.T) {
	resetLedgers(t)
	k := func(n string) ServerKey { return ServerKey{Name: n} }
	t0, t1 := time.Unix(1, 0), time.Unix(2, 0)
	mcpMu.Lock()
	clear(mcpServers)
	serverScopeKeys[k("work-connecting")] = "profile:work"
	serverScopeKeys[k("work-failed")] = "profile:work"
	serverScopeKeys[k("other-failed")] = "profile:other"
	serverConnecting[k("work-connecting")] = struct{}{}
	serverConnectErrors[k("work-failed")] = "work error"
	serverConnectErrors[k("other-failed")] = "other error"
	serverConnectRetryAfter[k("work-failed")] = t0
	serverConnectRetryAfter[k("other-failed")] = t1
	serverConnectFailures[k("work-failed")] = 1
	serverConnectFailures[k("other-failed")] = 2
	mcpMu.Unlock()
	t.Cleanup(func() {
		mcpMu.Lock()
		clear(serverScopeKeys)
		clear(serverConnecting)
		clear(serverConnectErrors)
		clear(serverConnectRetryAfter)
		clear(serverConnectFailures)
		mcpMu.Unlock()
	})
	scope := "profile:work"
	ShutdownMCPServers(context.Background(), &scope, nil, time.Second)
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if len(serverConnecting) != 0 ||
		!reflect.DeepEqual(serverConnectErrors, keyedMap[string]{k("other-failed"): "other error"}) ||
		!reflect.DeepEqual(serverScopeKeys, keyedMap[string]{k("other-failed"): "profile:other"}) ||
		!reflect.DeepEqual(serverConnectRetryAfter, keyedMap[time.Time]{k("other-failed"): t1}) ||
		!reflect.DeepEqual(serverConnectFailures, keyedMap[int]{k("other-failed"): 2}) {
		t.Fatalf("connecting %v errors %v scopes %v retry %v failures %v", serverConnecting, serverConnectErrors, serverScopeKeys,
			serverConnectRetryAfter, serverConnectFailures)
	}
}

func TestRegisterMcpServersClearsStaleConnectingOnTimeout(t *testing.T) {
	_, env := managerHome(t)
	prev := discoveryWaveTimeout
	discoveryWaveTimeout = 300 * time.Millisecond
	t.Cleanup(func() { discoveryWaveTimeout = prev })
	mcpMu.Lock()
	serverConnecting[ServerKey{Name: "srv_a"}] = struct{}{}
	mcpMu.Unlock()
	t.Cleanup(func() {
		mcpMu.Lock()
		clear(serverConnecting)
		clear(serverConnectErrors)
		mcpMu.Unlock()
	})
	servers := map[string]*Rendered{
		"srv_a": mockEntry(t, map[string]any{"args": []any{"a"}}),
		"srv_b": mockEntry(t, map[string]any{"args": []any{"b"}, "env": map[string]any{"MOCK_MCP_HANG_INIT": "1"}}),
	}
	_, err := Connect(context.Background(), env, servers)
	var timeoutErr *LoopTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("pass error %v", err)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	_, bStale := serverConnecting[ServerKey{Name: "srv_b"}]
	_, aKept := serverConnecting[ServerKey{Name: "srv_a"}]
	if bStale || !aKept {
		t.Fatalf("srv_b stale %v srv_a kept %v", bStale, aKept)
	}
}
