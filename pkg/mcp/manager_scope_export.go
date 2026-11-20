package mcp

import "context"

// Registry scope key of the MCP servers bound to the profile of ctx
func RegistryScope(ctx context.Context) string { return mcpRegistryScope(ctx) }
