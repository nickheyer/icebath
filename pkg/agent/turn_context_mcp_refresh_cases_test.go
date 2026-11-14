package agent

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestBetweenTurnsRefreshAddsLateToolWhenServersRegistered(t *testing.T) {
	a := newFixtureAgentWithConfig(t, "tools:\n  tool_search:\n    enabled: \"off\"\n", AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", Platform: "cli"})
	t.Cleanup(func() { a.Close() })
	a.sessionID, a.cachedSystemPrompt, a.compressionEnabled = "sess-1", "SYSTEM", false
	a.enabledToolsets, a.disabledToolsets = nil, nil
	cfg, err := config.Load(a.env.Home, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	a.env.Config = cfg
	if a.validToolNames["mcp_x_tool"] {
		t.Fatal("the late tool was already in the build time snapshot")
	}
	tools.Default.Register(tools.Def{Name: "mcp_x_tool", Toolset: "mcp-x",
		Schema:  map[string]any{"name": "mcp_x_tool", "description": "", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		Handler: func(context.Context, *tools.Call) string { return "{}" }})
	mcp.TrackMCPToolServer("mcp_x_tool", "x")
	t.Cleanup(func() {
		mcp.ForgetMCPToolServer("mcp_x_tool")
		_ = tools.Default.Deregister("mcp_x_tool", "", "")
	})
	prologueBuild(t, a, nil)
	listed := false
	for _, name := range mcp.AgentToolNames(a) {
		listed = listed || name == "mcp_x_tool"
	}
	if !a.validToolNames["mcp_x_tool"] || !listed {
		t.Fatalf("late tool valid %v listed %v in %v", a.validToolNames["mcp_x_tool"], listed, mcp.AgentToolNames(a))
	}
}
