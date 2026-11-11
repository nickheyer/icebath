package mcp

import "context"

// Reports configured MCP servers or enabled plugin MCP packages, true when the config cannot be read
func HasConfiguredServers(ctx context.Context) bool { return hasConfiguredServers(ctx) }
