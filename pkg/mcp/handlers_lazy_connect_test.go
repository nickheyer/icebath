package mcp

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Records a lazy mock server the way a cache hit registration leaves it
func lazyMockServer(t *testing.T, name string) {
	t.Helper()
	cfg := map[string]any{"command": mockBinary(t), "args": []any{}, "lazy": true, "timeout": 5,
		"env": map[string]any{"MOCK_MCP_CAPS": `{"resources":{},"prompts":{}}`}}
	mcpMu.Lock()
	lazyServerConfigs[ServerKey{Name: name}] = cfg
	lazyServerFingerprints[ServerKey{Name: name}] = "abc"
	mcpMu.Unlock()
	t.Cleanup(func() {
		mcpMu.Lock()
		delete(lazyServerConfigs, ServerKey{Name: name})
		delete(lazyServerFingerprints, ServerKey{Name: name})
		mcpMu.Unlock()
	})
}

// Runs one handler against a lazily registered mock server and asserts the first use connected it
func lazyFirstUseCase(t *testing.T, handler func(env *tools.Env) string, check func(t *testing.T, out map[string]any)) {
	t.Helper()
	_, env := managerHome(t)
	resetHandlerLedgers(t)
	lazyMockServer(t, "playwright")
	check(t, decodeResult(t, handler(env)))
	mcpMu.Lock()
	_, stillLazy := lazyServerConfigs[ServerKey{Name: "playwright"}]
	srv := mcpServers[ServerKey{Name: "playwright"}]
	mcpMu.Unlock()
	if stillLazy || srv == nil || srv.currentSession() == nil {
		t.Fatalf("first use must connect the lazy server, lazy %v server %v", stillLazy, srv)
	}
}

// test_mcp_lazy_start.py TestLazyFirstUseConnect test_tool_handler_lazy_connects_on_first_call
func TestLazyFirstUseConnectToolHandlerLazyConnectsOnFirstCall(t *testing.T) {
	lazyFirstUseCase(t, func(env *tools.Env) string {
		return makeToolHandler("playwright", "browser_navigate", 5, false)(context.Background(), &tools.Call{Name: "mcp__playwright__browser_navigate", Args: map[string]any{}, TaskID: "t1", Env: env})
	}, func(t *testing.T, out map[string]any) {
		if _, failed := out["error"]; failed || out["result"] != "" {
			t.Fatalf("tool %v", out)
		}
	})
}

// test_mcp_lazy_start.py TestLazyFirstUseConnect test_list_resources_handler_lazy_connects_on_first_call
func TestLazyFirstUseConnectListResourcesHandlerLazyConnectsOnFirstCall(t *testing.T) {
	lazyFirstUseCase(t, func(env *tools.Env) string {
		return makeListResourcesHandler("playwright", 5)(context.Background(), &tools.Call{Args: map[string]any{}, Env: env})
	}, func(t *testing.T, out map[string]any) {
		resources, _ := out["resources"].([]any)
		if _, failed := out["error"]; failed || len(resources) == 0 || resources[0].(map[string]any)["uri"] != "file:///a" {
			t.Fatalf("resources %v", out)
		}
	})
}

// test_mcp_lazy_start.py TestLazyFirstUseConnect test_get_prompt_handler_lazy_connects_on_first_call
func TestLazyFirstUseConnectGetPromptHandlerLazyConnectsOnFirstCall(t *testing.T) {
	lazyFirstUseCase(t, func(env *tools.Env) string {
		return makeGetPromptHandler("playwright", 5)(context.Background(), &tools.Call{Args: map[string]any{"name": "greeting"}, Env: env})
	}, func(t *testing.T, out map[string]any) {
		if _, failed := out["error"]; failed {
			t.Fatalf("prompt %v", out)
		}
	})
}

// test_mcp_lazy_start.py TestLazyFirstUseConnect test_lazy_connect_respects_connect_cooldown
func TestLazyFirstUseConnectLazyConnectRespectsConnectCooldown(t *testing.T) {
	_, env := managerHome(t)
	resetHandlerLedgers(t)
	lazyMockServer(t, "playwright")
	ctx := context.Background()
	mcpMu.Lock()
	recordConnectFailure(ctx, "playwright")
	mcpMu.Unlock()
	if ensureLazyServerConnected(ctx, env, "playwright") {
		t.Fatal("a lazy connect inside the cooldown reported a session")
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if _, spawned := mcpServers[ServerKey{Name: "playwright"}]; spawned {
		t.Fatal("the cooldown must stop the lazy connect from spawning the server")
	}
}
