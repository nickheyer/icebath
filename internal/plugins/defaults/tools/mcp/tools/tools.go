package tools

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	launchconfig "github.com/nickheyer/icebath/internal/launch/config"
	"github.com/nickheyer/icebath/internal/mcp"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/tools"
)

// Name of the tool that connects the user to MCP servers
const Manage = "manage_connections"

// Registers the tools of every profile's MCP servers and manage_connections; the engine behind them, for the plugin's other parts
func Register(registry *tools.Registry, profiles *profile.Registry, cfg *config.Config, catalog *mcp.Registry, helpers Helpers, owner mcp.Owner) *Manager {
	m := newManager(cfg, catalog, helpers, owner)
	registry.Register(m)
	profiles.Register(m)
	return m
}

// Tools of every server of every profile, each offered to the sessions of its own profile, and manage_connections
func (m *Manager) Tools() []tools.Def {
	defs := []tools.Def{m.manageDef()}
	m.mu.Lock()
	pools := make([]*pool, 0, len(m.pools))
	for _, p := range m.pools {
		pools = append(pools, p)
	}
	m.mu.Unlock()
	for _, p := range pools {
		defs = append(defs, p.definitions()...)
	}
	return defs
}

// Starts the servers of the session's profile connecting, unless they run, and gives them a bounded while to come up before the tools are
// listed: tools.mcp.discovery_timeout, or the one-shot wait for a query that ends the session
func (m *Manager) Prepare(env *tools.Env) {
	if env == nil || env.Config == nil || safe(env.Config) {
		return
	}
	p := m.pool(env.Config)
	run := p.discover(env.Cwd)
	key, fallback := mcpconfig.DiscoveryTimeout, 1.5
	if env.OneShot {
		key, fallback = mcpconfig.SingleQueryTimeout, 15
	}
	wait, ok := seconds(env.Config.Get(key))
	if !ok || wait <= 0 {
		wait = fallback
	}
	t := time.NewTimer(duration(wait))
	defer t.Stop()
	select {
	case <-run.done:
	case <-t.C:
	}
}

// Whether the settings, those of a session over its profile's, keep every MCP server away, as a launch in safe mode asks of its sessions
func safe(cfg *config.Config) bool {
	return cfg.GetBool(launchconfig.SafeMode)
}

// Starts the servers of the profile connecting, for an app that opened so its first turn finds them ready
func (m *Manager) Start(cfg *config.Config) {
	m.pool(cfg).discover("")
}

// Tools icebath serves to MCP clients with mcp serve --tools, never one that runs commands, writes files, runs code or delegates
var Exposed = []string{"web_search", "web_extract", "browser_navigate", "browser_click", "browser_type", "browser_press", "browser_snapshot",
	"browser_scroll", "browser_back", "browser_get_images", "browser_console", "browser_vision", "vision_analyze", "image_generate", "skill_view",
	"skills_list", "text_to_speech"}

func (m *Manager) manageDef() tools.Def {
	return tools.Def{
		Name:    Manage,
		Toolset: "connections",
		Title:   "Connect",
		About:   "Add MCP servers, turn them on and sign in to them",
		Group:   "MCP servers",
		Icon:    tools.IconServer,
		Schema: map[string]any{
			"description": "Connect the user to MCP servers: a server from the bundled catalog or one already in the user's MCP server settings, each with " +
				"its own credentials and OAuth. 'install' adds a catalog entry, 'enable' re-enables a disabled configured server, 'authorize' runs its OAuth. " +
				"Pass SEVERAL names in one call. In the terminal app the call shows the user a connection card and blocks until every server is connected, " +
				"skipped, or the deadline passes. Where no card exists (a one-shot run, a scheduled job, another client) each target runs at once and the result " +
				"says what happened, with a link for the USER to open in a browser when one is needed (never open it yourself). Never hand-edit the MCP server " +
				"settings — always use this tool. After a skip or a timeout, do not re-ask on your own: continue without the server or ask in chat. A later " +
				"request from the USER for that same server is not a re-ask — run it. A connected server's tools are named in the result and are callable on " +
				"your next step. This tool can NOT remove a server or revoke its authorization — that is deliberately user-only: direct the user to `icebath mcp` " +
				"or /mcp.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "enum": []any{"install", "enable", "authorize"},
						"description": "install takes catalog entry names; enable and authorize take configured server names."},
					"connectors": map[string]any{
						"type": "array",
						"items": map[string]any{"anyOf": []any{
							map[string]any{"type": "string"},
							map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []any{"name"},
								"additionalProperties": false},
						}},
						"description": "The MCP server names, e.g. [\"linear\", \"deepwiki\"].",
					},
				},
				"required": []string{"action", "connectors"},
			},
		},
		Handler: func(ctx context.Context, call *tools.Call) string { return m.manage(ctx, call) },
		// A session in safe mode connects no server, so it is offered no way to
		Check: func(env *tools.Env) bool { return env == nil || env.Config == nil || !safe(env.Config) },
		// It waits on the user and its own deadline, so no other limit cuts it short
		Timeout: -1,
	}
}
