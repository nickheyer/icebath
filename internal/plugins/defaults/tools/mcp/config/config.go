package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key = toolsconfig.Key + ".mcp"
	// Servers the agent connects to, by name
	Servers = Key + ".servers"
	// Values server settings name as ${NAME}, by name
	Env = Key + ".env"
	// Folder of catalog manifests used in place of the built-in catalog
	CatalogDir = Key + ".catalog_dir"
	// Servers connecting at the same time
	DiscoveryConcurrency = Key + ".discovery_concurrency"
	// Seconds a turn waits for servers to connect, and a one-shot query
	DiscoveryTimeout   = Key + ".discovery_timeout"
	SingleQueryTimeout = Key + ".single_query_discovery_timeout"
	// Seconds one call of a server's tool may run when the server sets no timeout of its own
	ToolTimeout = Key + ".tool_call_timeout"
	// Whether /reload-mcp asks before reconnecting the servers
	ReloadConfirm = Key + ".reload_confirm"
)

// Helper task that answers the sampling requests of servers
const Task = "mcp"
