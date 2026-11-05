package mcp

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func TestToolHandlerRecycledStdioReconnectsLazily(t *testing.T) {
	srv, _ := handlerServer(t, "test_srv", func(string, map[string]any) (any, error) { return textResult("stale"), nil })
	srv.setSession(nil)
	srv.mu.Lock()
	srv.config = map[string]any{"command": "npx"}
	srv.recycledReason = "idle_timeout_seconds"
	srv.mu.Unlock()
	var mu sync.Mutex
	var seen []map[string]any
	var swaps atomic.Int32
	serveReconnects(t, srv, func(method string, params map[string]any) (any, error) {
		if method == methodToolsCall {
			mu.Lock()
			seen = append(seen, params)
			mu.Unlock()
		}
		return textResult("reconnected"), nil
	}, func() {
		swaps.Add(1)
		srv.mu.Lock()
		srv.recycledReason = ""
		srv.mu.Unlock()
	})
	out := makeToolHandler("test_srv", "greet", 120, false)(context.Background(), toolCall(map[string]any{"name": "world"}))
	if decodeResult(t, out)["result"] != "reconnected" {
		t.Fatalf("got %s", out)
	}
	if swaps.Load() != 1 {
		t.Fatalf("lazy reconnects %d", swaps.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	want := []map[string]any{{"name": "greet", "arguments": map[string]any{"name": "world"}}}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("call_tool params %v", seen)
	}
}
