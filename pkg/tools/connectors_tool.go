package tools

import (
	"strings"

	"context"
)

// ManageOptions are manage_connections' keyword arguments, empty strings standing for None
type ManageOptions struct {
	MCPBackend any
	SessionID  string
	ToolCallID string
	Callback   ConnCallback
}

// Runs one manage_connections call over the named MCP servers
func ManageConnections(ctx context.Context, env *Env, args map[string]any, opts ManageOptions) string {
	action := strings.ToLower(strings.TrimSpace(connStrOr(args["action"])))
	names, targetErr := NormalizeTargets(args["connectors"])
	if targetErr != "" {
		return toolErrorJSON(targetErr, nil)
	}
	if msg := ValidateAction(action, names); msg != "" {
		return toolErrorJSON(msg, nil)
	}
	return MCPOperationRunner(ctx, env, names, action, MCPOperationOptions{Callback: opts.Callback,
		SessionID: opts.SessionID, ToolCallID: opts.ToolCallID, Backend: opts.MCPBackend})
}

func connActionEnum() []any {
	out := make([]any, len(MCPActions))
	for i, a := range MCPActions {
		out[i] = a
	}
	return out
}

// ManageConnectionsSchema is the manage_connections tool schema the model sees
var ManageConnectionsSchema = Dict(
	"name", "manage_connections",
	"description", "Connect the user to MCP servers: a server from the bundled catalog or one already in the user's "+
		"mcp_servers config, each with its own credentials and OAuth. 'install' adds a catalog entry, "+
		"'enable' re-enables a disabled configured server, 'authorize' runs its OAuth. Pass SEVERAL names in one call. "+
		"In the terminal UI the call shows the user an approval card and blocks until "+
		"every server is connected, skipped, or the deadline passes. Where no card exists (a one-shot run, a "+
		"scheduled job, a messaging platform) each target runs at once and the result says what happened, with a "+
		"link for the USER to open in a browser when one is needed (never open it yourself). Never hand-edit "+
		"mcp_servers config — always use this tool. After a skip or a timeout, do not re-ask on "+
		"your own: continue without the server or ask in chat. A later request from the USER for that "+
		"same server is not a re-ask — run it. A connected server's tools are named in the result and are "+
		"callable at once through tool_describe/tool_call. This tool can NOT remove a server or revoke its "+
		"authorization — that is deliberately user-only: direct the user to `icebath mcp`.",
	"parameters", Dict(
		"type", "object",
		"properties", Dict(
			"action", Dict(
				"type", "string",
				"enum", connActionEnum(),
				"description", "install takes catalog entry names; enable and authorize take configured server names.",
			),
			"connectors", Dict(
				"type", "array",
				"items", Dict(
					"anyOf", []any{
						Dict("type", "string"),
						Dict(
							"type", "object",
							"properties", Dict("name", Dict("type", "string")),
							"required", []any{"name"},
							"additionalProperties", false,
						),
					},
				),
				"description", "The MCP server names, e.g. [\"linear\", \"deepwiki\"].",
			),
		),
		"required", []any{"action", "connectors"},
	),
)

func init() {
	Register(Def{Name: "manage_connections", Toolset: "connections", Schema: ManageConnectionsSchema, Emoji: "🔗",
		Handler: func(ctx context.Context, call *Call) string {
			return ManageConnections(ctx, call.Env, call.Args, ManageOptions{SessionID: call.SessionID, ToolCallID: call.ToolCallID})
		}})
}

// SideAgentToolDrops are the tools a delegated side agent never sees, only the main agent draws connection cards
var SideAgentToolDrops = map[string]bool{"manage_connections": true}
