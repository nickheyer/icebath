package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

type staleRefreshCalls struct {
	builder   []string
	refreshes []mcp.RefreshOptions
	pinned    int
}

// Runs the stale Bot Chat restore of one surface, recording the surface builder, the tool refresh and the pin
func runStaleRefresh(t *testing.T, platform string, policy gateway.ActiveLaunchPolicy) *staleRefreshCalls {
	t.Helper()
	row := pyjson.NewObject()
	row.Set("system_prompt", "SYSTEM PROMPT BODY\n\nConversation started: Monday, January 05, 2026\nModel: test-model\nProvider: openrouter\nPlatform: desktop")
	row.Set("tool_names", nil)
	a, _ := restoreAgent(t, restoreRowStore(t, row), "NEW_PROMPT")
	a.platform, a.passSessionID, a.sessionTitleHint = platform, false, "Bot Chat"
	a.enabledToolsets = []string{"web"}
	a.tools = []any{restoreToolDef("web_search")}
	a.validToolNames = map[string]bool{"web_search": true}
	calls := &staleRefreshCalls{}
	prevStale, prevRefresh, prevPin, prevBuilder := loopStoredPromptCapabilityStale, loopRefreshAgentMCPTools, loopPersistAgentToolNames, TUIGatewayToolsets
	t.Cleanup(func() {
		loopStoredPromptCapabilityStale, loopRefreshAgentMCPTools, loopPersistAgentToolNames, TUIGatewayToolsets = prevStale, prevRefresh, prevPin, prevBuilder
	})
	loopStoredPromptCapabilityStale = func(string, string) bool { return true }
	loopRefreshAgentMCPTools = func(_ context.Context, _ mcp.AgentToolSnapshot, opts mcp.RefreshOptions) map[string]bool {
		calls.refreshes = append(calls.refreshes, opts)
		return map[string]bool{}
	}
	loopPersistAgentToolNames = func(context.Context, mcp.AgentToolSnapshot) { calls.pinned++ }
	TUIGatewayToolsets = func(_ context.Context, surface string) ([]string, []string) {
		calls.builder = append(calls.builder, surface)
		return []string{"terminal", "file", "desktop_ui"}, []string{"computer_use"}
	}
	ctx := tools.WithToolEnv(context.Background(), a.env)
	if policy != nil {
		ctx = gateway.WithActivePolicy(ctx, policy)
	}
	if err := restoreOrBuildSystemPrompt(ctx, a, nil, hiHistory()); err != nil {
		t.Fatal(err)
	}
	if a.cachedSystemPrompt != "NEW_PROMPT" {
		t.Fatalf("cached prompt %q", a.cachedSystemPrompt)
	}
	return calls
}

func TestStaleEpochRebuildsToolsThroughTheSurfaceBuilder(t *testing.T) {
	calls := runStaleRefresh(t, "desktop", nil)
	if !reflect.DeepEqual(calls.builder, []string{"desktop"}) || len(calls.refreshes) != 1 {
		t.Fatalf("builder %v refreshes %v", calls.builder, calls.refreshes)
	}
	opts := calls.refreshes[0]
	if !reflect.DeepEqual(opts.EnabledOverride, []string{"terminal", "file", "desktop_ui"}) || !reflect.DeepEqual(opts.DisabledOverride, []string{"computer_use"}) {
		t.Fatalf("overrides %+v", opts)
	}
	if opts.PreservePrefix || calls.pinned == 0 {
		t.Fatalf("preserve prefix %v pins %d", opts.PreservePrefix, calls.pinned)
	}
}

func TestStaleEpochLeavesAPerProcessSurfaceAlone(t *testing.T) {
	calls := runStaleRefresh(t, "cli", nil)
	if len(calls.builder) != 0 || len(calls.refreshes) != 0 {
		t.Fatalf("builder %v refreshes %v", calls.builder, calls.refreshes)
	}
}

func TestFrozenSessionPolicyWinsOverLiveSurfaceConfig(t *testing.T) {
	calls := runStaleRefresh(t, "desktop", &gateway.LocalSessionPolicy{Toolsets: []string{"file", "web"}})
	if len(calls.builder) != 0 || len(calls.refreshes) != 1 {
		t.Fatalf("builder %v refreshes %v", calls.builder, calls.refreshes)
	}
	if opts := calls.refreshes[0]; !reflect.DeepEqual(opts.EnabledOverride, []string{"file", "web"}) || opts.DisabledOverride != nil {
		t.Fatalf("overrides %+v", opts)
	}
}
