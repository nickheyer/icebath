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

func TestHandlersLazyConnectOnFirstUse(t *testing.T) {
	cases := []struct {
		name    string
		handler func(env *tools.Env) string
		check   func(t *testing.T, out map[string]any)
	}{
		{"tool", func(env *tools.Env) string {
			return makeToolHandler("playwright", "browser_navigate", 5, false)(context.Background(), &tools.Call{Name: "mcp__playwright__browser_navigate", Args: map[string]any{}, TaskID: "t1", Env: env})
		}, func(t *testing.T, out map[string]any) {
			if _, failed := out["error"]; failed || out["result"] != "" {
				t.Fatalf("tool %v", out)
			}
		}},
		{"list_resources", func(env *tools.Env) string {
			return makeListResourcesHandler("playwright", 5)(context.Background(), &tools.Call{Args: map[string]any{}, Env: env})
		}, func(t *testing.T, out map[string]any) {
			resources, _ := out["resources"].([]any)
			if _, failed := out["error"]; failed || len(resources) == 0 || resources[0].(map[string]any)["uri"] != "file:///a" {
				t.Fatalf("resources %v", out)
			}
		}},
		{"get_prompt", func(env *tools.Env) string {
			return makeGetPromptHandler("playwright", 5)(context.Background(), &tools.Call{Args: map[string]any{"name": "greeting"}, Env: env})
		}, func(t *testing.T, out map[string]any) {
			if _, failed := out["error"]; failed {
				t.Fatalf("prompt %v", out)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, env := managerHome(t)
			resetHandlerLedgers(t)
			lazyMockServer(t, "playwright")
			tc.check(t, decodeResult(t, tc.handler(env)))
			mcpMu.Lock()
			_, stillLazy := lazyServerConfigs[ServerKey{Name: "playwright"}]
			srv := mcpServers[ServerKey{Name: "playwright"}]
			mcpMu.Unlock()
			if stillLazy || srv == nil || srv.currentSession() == nil {
				t.Fatalf("first use must connect the lazy server, lazy %v server %v", stillLazy, srv)
			}
		})
	}
}
