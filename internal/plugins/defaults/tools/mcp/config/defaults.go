package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "MCP",
		About:  "Tool servers the agent connects to over the Model Context Protocol, how long they get, and the catalog they come from",
		Settings: []config.Setting{
			{Key: Servers, Title: "MCP servers", About: "Tool servers the agent connects to, by name: command, args, env (a list of NAME=value) and cwd for one it " +
				"starts, or url, headers and auth for a remote one; enabled, timeout, connect_timeout, lazy, trust, tools (include, exclude, resources, prompts), " +
				"oauth, sampling, elicitation and the rest. Manage them with /mcp or icebath mcp", Default: map[string]any{}},
			{Key: Env, Title: "MCP values", About: "Values the servers' settings name as ${NAME}, such as their keys, by name; the environment wins over them",
				Default: map[string]any{}},
			{Key: CatalogDir, Title: "MCP catalog folder", About: "Folder of MCP catalog manifests used in place of the built-in catalog; empty uses the built-in one",
				Default: "", Env: []string{"ICEBATH_OPTIONAL_MCPS"}},
			{Key: DiscoveryConcurrency, Title: "MCP connect limit", About: "How many MCP servers connect at the same time; 0 for no limit", Default: 4},
			{Key: DiscoveryTimeout, Title: "MCP startup wait", About: "Seconds a new turn waits for MCP servers to connect; 0 or less uses 1.5", Default: 1500 * time.Millisecond},
			{Key: SingleQueryTimeout, Title: "MCP one-shot wait", About: "Seconds a one-shot query waits for MCP servers to connect; 0 or less uses 15",
				Default: 15 * time.Second},
			{Key: ToolTimeout, Title: "MCP tool call limit", About: "Seconds one call of an MCP tool may take; a server's own timeout wins over it; 0 or less uses 300",
				Default: 5 * time.Minute},
			{Key: ReloadConfirm, Title: "Confirm tool reload", About: "Ask before /reload-mcp reconnects the MCP servers; /reload-mcp now never asks", Default: true},
		},
	}})
}
