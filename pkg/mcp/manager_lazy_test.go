package mcp

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestLazyCacheMissFallsBackToEagerConnect(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	names, err := Connect(ctx, env, map[string]*Rendered{"lz": mockEntry(t, map[string]any{"lazy": true})})
	if err != nil || !slices.Contains(names, "mcp__lz__tool0") {
		t.Fatalf("names %v err %v", names, err)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	_, lazy := lazyServerConfigs[ServerKey{Name: "lz"}]
	if srv := mcpServers[ServerKey{Name: "lz"}]; srv == nil || lazy {
		t.Fatalf("a cache miss must connect eagerly, server %v lazy %v", srv, lazy)
	}
}

func TestLazyNonLazyServerNeverTouchesCache(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	r := mockEntry(t, nil)
	if err := WriteCacheEntry(ctx, "eager", ConfigFingerprint(r.Config), []any{map[string]any{"name": "cached_only"}}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	names, err := Connect(ctx, env, map[string]*Rendered{"eager": r})
	if err != nil || slices.Contains(names, "mcp__eager__cached_only") || !slices.Contains(names, "mcp__eager__tool0") {
		t.Fatalf("names %v err %v", names, err)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if _, lazy := lazyServerConfigs[ServerKey{Name: "eager"}]; lazy || mcpServers[ServerKey{Name: "eager"}] == nil {
		t.Fatal("a non lazy server must spawn and never register from the cache")
	}
}

func TestLazyServerNotReregisteredOnSecondPass(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	r := mockEntry(t, map[string]any{"lazy": true})
	key := ServerKey{Name: "playwright"}
	mcpMu.Lock()
	lazyServerConfigs[key] = r.Config
	lazyServerToolNames[key] = []string{"mcp_playwright_browser_navigate"}
	mcpToolServerNames["mcp_playwright_browser_navigate"] = "playwright"
	mcpMu.Unlock()
	t.Cleanup(func() {
		mcpMu.Lock()
		delete(lazyServerConfigs, key)
		delete(lazyServerToolNames, key)
		mcpMu.Unlock()
	})
	names, err := Connect(ctx, env, map[string]*Rendered{"playwright": r})
	if err != nil || !slices.Contains(names, "mcp_playwright_browser_navigate") {
		t.Fatalf("names %v err %v", names, err)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if mcpServers[key] != nil || len(lazyServerToolNames[key]) != 1 {
		t.Fatalf("second pass respawned or reregistered %v %v", mcpServers[key], lazyServerToolNames[key])
	}
}

func TestReconcileRetriesFailedFirstConnectOnlyAfterCooldown(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	writeServersConfig(t, "  ghost:\n    url: http://127.0.0.1:1/mcp\n")
	noteConnectFailure(ctx, "ghost", errors.New("connection refused"))
	res, err := Reconcile(ctx, env)
	if err != nil || len(res.Added) != 0 {
		t.Fatalf("inside the cooldown nothing is attempted %+v %v", res, err)
	}
	mcpMu.Lock()
	clear(serverConnectRetryAfter)
	mcpMu.Unlock()
	res, err = Reconcile(ctx, env)
	if err != nil || !slices.Equal(res.Added, []string{"ghost"}) || !connectCooldownActive(ctx, "ghost") {
		t.Fatalf("cooldown lapsed, the never connected server goes through discovery %+v %v", res, err)
	}
	mcpMu.Lock()
	clear(serverConnectRetryAfter)
	serverConnecting[ServerKey{Name: "ghost"}] = struct{}{}
	mcpMu.Unlock()
	prev := discoveryWaveTimeout
	discoveryWaveTimeout = 200 * time.Millisecond
	t.Cleanup(func() { discoveryWaveTimeout = prev })
	hung := mockEntry(t, map[string]any{"env": map[string]any{"MOCK_MCP_HANG_INIT": "1"}})
	var timeoutErr *LoopTimeoutError
	if err := runDiscoveryPass(ctx, env, map[string]*Rendered{"ghost": hung}); !errors.As(err, &timeoutErr) {
		t.Fatalf("pass error %v", err)
	}
	if !connectCooldownActive(ctx, "ghost") {
		t.Fatal("a discovery pass timeout must stamp the connect cooldown")
	}
}
