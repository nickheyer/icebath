package agent

import (
	"context"
	"fmt"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	tools.RegisterInlineToolExecutor("todo_list", agentInlineExecutor("todo_list", func(a *AIAgent, _ context.Context, args map[string]any, _ tools.InlineToolContext) (string, error) {
		return tools.RunTaskList(a.todoStore, args), nil
	}))
	tools.RegisterInlineToolExecutor("manage_connections", agentInlineExecutor("manage_connections", manageConnectionsInline))
	tools.RegisterInlineToolExecutor("manage_catalog", agentInlineExecutor("manage_catalog", func(a *AIAgent, ctx context.Context, args map[string]any, ictx tools.InlineToolContext) (string, error) {
		callback, err := connectionCallbackOf(a.connectionCallback)
		if err != nil {
			return "", err
		}
		return mcp.ManageCatalog(ctx, a.env, args, mcp.CatalogOptions{SessionID: a.sessionID, ToolCallID: ictx.ToolCallID,
			Callback: callback, CardSurface: a.platform == "desktop"})
	}))
	tools.RegisterInlineToolExecutor("setup_mcp", agentInlineExecutor("setup_mcp", func(a *AIAgent, ctx context.Context, args map[string]any, ictx tools.InlineToolContext) (string, error) {
		action, ok := args["action"]
		if !ok {
			action = "install"
		}
		server, ok := args["server"]
		if !ok {
			server = ""
		}
		return manageConnectionsInline(a, ctx, map[string]any{"action": action,
			"connectors": []any{map[string]any{"name": server, "mcp": true}}}, ictx)
	}))
}

// Executor over the agent behind the executor host, an error on any other host
func agentInlineExecutor(name string, run func(a *AIAgent, ctx context.Context, args map[string]any, ictx tools.InlineToolContext) (string, error)) tools.InlineToolExecutor {
	return func(ctx context.Context, host tools.ExecutorHost, args map[string]any, ictx tools.InlineToolContext) (string, error) {
		h, ok := host.(agentExecutorHost)
		if !ok || h.AIAgent == nil {
			return "", fmt.Errorf("the %s tool runs on an agent executor, not %T", name, host)
		}
		return run(h.AIAgent, ctx, args, ictx)
	}
}

// Connection request callback of the agent as the card callback manage_connections drives, nil when it carries none
func connectionCallbackOf(cb any) (tools.ConnCallback, error) {
	switch fn := cb.(type) {
	case nil:
		return nil, nil
	case tools.ConnCallback:
		return fn, nil
	case func(payload *pyjson.Object) string:
		return fn, nil
	case func(payload map[string]any):
		return func(payload *pyjson.Object) string {
			plain, _ := pyjson.Plain(payload).(map[string]any)
			fn(plain)
			return ""
		}, nil
	case func(args ...any):
		return func(payload *pyjson.Object) string {
			fn(payload)
			return ""
		}, nil
	}
	return nil, fmt.Errorf("connection_callback is a %T, not a connection request callback", cb)
}

// Runs manage_connections with the agent's card callback, then scopes newly connected MCP servers into the turn
func manageConnectionsInline(a *AIAgent, ctx context.Context, args map[string]any, ictx tools.InlineToolContext) (string, error) {
	callback, err := connectionCallbackOf(a.connectionCallback)
	if err != nil {
		return "", err
	}
	env := a.env
	result := tools.ManageConnections(ctx, env, args, tools.ManageOptions{SessionID: a.sessionID, ToolCallID: ictx.ToolCallID,
		TurnID: a.currentTurnID, Callback: callback, ConnectorsAvailable: func() bool { return tools.ConnectorsAvailable(env) },
		CardSurface: a.platform == "desktop"})
	if err := scopeInConnectedMCPServers(a, result); err != nil {
		return "", err
	}
	return result, nil
}

// Adds the MCP servers the result reports connected with tools to the agent's toolset selection
func scopeInConnectedMCPServers(a *AIAgent, result string) error {
	enabled := a.enabledToolsets
	if enabled == nil || slices.Contains(enabled, "no_mcp") {
		return nil
	}
	decoded, err := pyjson.Loads(result)
	if err != nil {
		return nil
	}
	obj, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil
	}
	raw, _ := obj.Get("targets")
	if !pyrepr.Truthy(raw) {
		return nil
	}
	var targets []any
	switch v := raw.(type) {
	case []any:
		targets = v
	case string, *pyjson.Object:
	default:
		return fmt.Errorf("TypeError: '%s' object is not iterable", pyrepr.TypeName(raw))
	}
	var added []string
	for _, item := range targets {
		t, ok := item.(*pyjson.Object)
		if !ok {
			continue
		}
		kind, _ := t.Get("kind")
		stateValue, _ := t.Get("state")
		toolList, _ := t.Get("tools")
		if kind != "mcp" || stateValue != "connected" || !pyrepr.Truthy(toolList) {
			continue
		}
		name, _ := t.Get("name")
		if text := pyrepr.Str(name); !slices.Contains(enabled, text) {
			added = append(added, text)
		}
	}
	if len(added) > 0 {
		a.enabledToolsets = append(slices.Clone(enabled), added...)
	}
	return nil
}
