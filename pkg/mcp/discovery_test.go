package mcp

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func mcpBatchCall(name, args, id string) state.ToolCall {
	return state.ToolCall{ID: id, Type: "function", Function: state.ToolCallFunction{Name: name, Arguments: args}}
}

func TestMCPToolsParallelWhenServerOptedIn(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	batch := []state.ToolCall{
		mcpBatchCall("mcp__github__list_repos", `{"org":"openai"}`, "c1"),
		mcpBatchCall("mcp__github__search_code", `{"q":"test"}`, "c2"),
	}
	own := serverKey(ctx, "github", "", true)
	mcpMu.Lock()
	parallelSafeServers[own] = struct{}{}
	mcpToolServerNames["mcp__github__list_repos"] = "github"
	mcpToolServerNames["mcp__github__search_code"] = "github"
	mcpMu.Unlock()
	t.Cleanup(func() {
		mcpMu.Lock()
		delete(parallelSafeServers, own)
		delete(mcpToolServerNames, "mcp__github__list_repos")
		delete(mcpToolServerNames, "mcp__github__search_code")
		mcpMu.Unlock()
	})
	if !tools.ShouldParallelizeToolBatch(ctx, nil, batch) {
		t.Fatal("a parallel opted in MCP server's batch must run concurrently")
	}
	mcpMu.Lock()
	delete(parallelSafeServers, own)
	mcpMu.Unlock()
	if tools.ShouldParallelizeToolBatch(ctx, nil, batch) {
		t.Fatal("MCP tools without the server opt in stay sequential")
	}
}
