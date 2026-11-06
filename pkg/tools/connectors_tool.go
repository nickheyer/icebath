package tools

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ManageOptions are manage_connections' keyword arguments, empty strings standing for None
type ManageOptions struct {
	ClientFactory       func() (ManagedClient, error)
	MCPBackend          any
	SessionID           string
	ToolCallID          string
	TurnID              string
	Callback            ConnCallback
	ConnectorsAvailable func() bool
	CardSurface         bool
}

// Routes one manage_connections call to the hosted connector path or the local MCP path
func ManageConnections(ctx context.Context, env *Env, args map[string]any, opts ManageOptions) string {
	action := "status"
	if connTruthy(args["action"]) {
		action = pystr.Lower(pystr.Strip(connStrOr(args["action"])))
	}
	managed, mcpTargets, targetErr := NormalizeTargets(args["connectors"])
	if targetErr != "" {
		return toolErrorJSON(targetErr, nil)
	}
	if msg := ValidateAction(action, managed, mcpTargets); msg != "" {
		return toolErrorJSON(msg, nil)
	}
	if slices.Contains(MCPActions, action) {
		return MCPOperationRunner(ctx, env, mcpTargets, action, MCPOperationOptions{Callback: opts.Callback,
			SessionID: opts.SessionID, ToolCallID: opts.ToolCallID, Backend: opts.MCPBackend})
	}
	return RunManagedAction(ctx, env, action, managed, args, ManagedOptions{ClientFactory: opts.ClientFactory,
		SessionID: opts.SessionID, ToolCallID: opts.ToolCallID, TurnID: opts.TurnID, Callback: opts.Callback,
		ConnectorsAvailable: opts.ConnectorsAvailable, CardSurface: opts.CardSurface})
}

func connActionEnum() []any {
	out := make([]any, len(AllActions))
	for i, a := range AllActions {
		out[i] = a
	}
	return out
}

// ManageConnectionsSchema is the manage_connections tool schema the model sees
var ManageConnectionsSchema = map[string]any{
	"name": "manage_connections",
	"description": "Connect the user to apps. Two kinds: a hosted connector account (Gmail, Notion, ...) " +
		"served through the tool gateway, and a local MCP server from the bundled catalog. " +
		"Targets go in 'connectors': a bare slug or {\"name\": \"gmail\"} is a hosted connector " +
		"account; {\"name\": \"linear\", \"mcp\": true} is a local MCP server. Many names exist on " +
		"both sides, so the user's own words decide. Pass \"mcp\": true only when the user asks " +
		"for an MCP server, a local server or an install, or when the name exists only as a " +
		"catalog entry; otherwise the target is hosted. 'connect' and 'reconnect' are hosted " +
		"actions; 'install', 'enable' and 'authorize' are MCP actions. A target the other side " +
		"owns is refused with the call that does work. " +
		"Hosted actions: 'display' (desktop only; elsewhere returns status) shows every connector and " +
		"waits for Continue (300 s); put slugs to suggest in 'connectors'. Returns connected/newly_connected " +
		"(+timed_out); the user saw the list, so do not repeat it. 'connect' is for an app the user names. " +
		"'status' lists connectors and whether each is connected; 'connect' " +
		"starts an authorization for the given connectors; 'reconnect' checks each one and " +
		"repairs only what is not connected ('force': true restarts even a working one, for an " +
		"account switch). A second account of one app is 'connect' with a new 'alias': pick a short " +
		"lowercase name from the user's words (e.g. 'work'), and ask the user if unsure. 'status' lists " +
		"each connector's accounts; 'rename' names or renames one. Pass SEVERAL slugs in one call. In the desktop app, the terminal UI and " +
		"the interactive CLI the call shows the user a card and blocks until every app is " +
		"connected, skipped, or the deadline passes; the result lists each target as connected / " +
		"skipped / not_connected and never carries a link. Where no card exists (a one-shot run, " +
		"a scheduled job, a messaging platform) the result carries a connect_url per app for the " +
		"USER to open in a browser (never open it yourself); ask them to say when they are done, " +
		"then use 'status'. " +
		"When a hosted connector tool call returns CONNECTION_REQUIRED, use 'connect'. " +
		"MCP actions (every target must carry \"mcp\": true): 'install' adds a catalog entry, " +
		"'enable' re-enables a disabled configured server, 'authorize' runs its OAuth. " +
		"They show the user an approval card and block until it settles. Never hand-edit " +
		"mcp_servers config — always use this tool. After a skip or a timeout, do not re-ask on " +
		"your own: continue without the app or ask in chat. A later request from the USER for that " +
		"same app is not a re-ask — run it. A connected server's tools are named in the result and are " +
		"callable at once through tool_describe/tool_call. Where no card exists an MCP target runs at once and the result says what " +
		"happened, with a link for the user to open when one is needed. This tool can NOT " +
		"disconnect, delete, or revoke an account — that is deliberately user-only. When asked, say so and direct the user to the " +
		"Nous Portal (their org's Connectors page) or the desktop app.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": connActionEnum(),
				"description": "Defaults to status. connect, reconnect and rename take hosted connector slugs only. " +
					"install, enable and authorize take mcp:true targets only.",
			},
			"connectors": map[string]any{
				"type": "array",
				"items": map[string]any{
					"anyOf": []any{
						map[string]any{"type": "string"},
						map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name": map[string]any{"type": "string"},
								"mcp": map[string]any{
									"type": "boolean",
									"description": "true = a local MCP server from the catalog; absent or " +
										"false = a hosted connector account.",
								},
								"alias": map[string]any{
									"type": "string",
									"description": "Hosted only: the account's name. connect: a new name adds another " +
										"account; reconnect: repairs that account; rename: its current name " +
										"(or its label when unnamed). One aliased target per call.",
								},
								"to": map[string]any{
									"type":        "string",
									"pattern":     AliasPattern,
									"description": "rename only: the new name.",
								},
							},
							"required":             []any{"name"},
							"additionalProperties": false,
						},
					},
				},
				"description": "Targets. REQUIRED for every action but status and display " +
					"(e.g. [\"gmail\", {\"name\": \"linear\", \"mcp\": true}]); optional filter for " +
					"status. A bare slug is a hosted connector, so an MCP server needs the object " +
					"form with \"mcp\": true.",
			},
			"force": map[string]any{
				"type":        "boolean",
				"description": "reconnect only: restart the authorization even if the app is connected (account switch).",
			},
		},
		"required": []any{},
	},
}

func init() {
	Register(Def{Name: "manage_connections", Toolset: "connections", Schema: ManageConnectionsSchema, Emoji: "🔗",
		Check: ConnectorsAvailable,
		Handler: func(ctx context.Context, call *Call) string {
			env := call.Env
			return ManageConnections(ctx, env, call.Args, ManageOptions{SessionID: call.SessionID, ToolCallID: call.ToolCallID,
				ConnectorsAvailable: func() bool { return ConnectorsAvailable(env) }})
		}})
}
