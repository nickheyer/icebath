package mcp

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Registers a tool under a toolset in the default registry for the life of the test
func registerForTest(t *testing.T, name, toolset string) {
	tools.Default.Remove(name)
	tools.Default.Register(testToolDef(name, toolset))
	t.Cleanup(func() { tools.Default.Remove(name) })
}

func TestHealthRefreshToolsSkips(t *testing.T) {
	logs := captureLogs(t)
	ctx := context.Background()
	idle := &serverTask{name: "idle", toolNames: []string{"mcp__idle__a"}}
	if err := idle.refreshTools(ctx); err != nil || logs.count("MCP server 'idle': skipping dynamic tool refresh; session not connected") != 1 {
		t.Fatalf("disconnected refresh %v %q", err, logs.text())
	}
	if got := idle.registeredToolNames(); len(got) != 1 || idle.rpc.Locked() {
		t.Fatalf("skipped refresh changes nothing and releases the rpc lock: %v", got)
	}
	silent := &serverTask{name: "notools", initializeResult: &InitializeResult{Capabilities: &ServerCapabilities{}}}
	if err := silent.refreshTools(ctx); err != nil || logs.count("notools") != 0 {
		t.Fatalf("a server without the tools capability is never listed %v %q", err, logs.text())
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	busy := &serverTask{name: "busy"}
	if err := busy.rpc.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := busy.refreshTools(cancelled); err == nil {
		t.Fatal("a refresh queued behind a held rpc lock ends with its context")
	}
	busy.rpc.Unlock()
}

// Minimal registrable tool definition
func testToolDef(name, toolset string) tools.Def {
	return tools.Def{Name: name, Toolset: toolset, Schema: map[string]any{"name": name},
		Handler: func(context.Context, *tools.Call) string { return "{}" }}
}
