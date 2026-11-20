package mcp

import "context"

// The mcp_servers mapping of the home user config, empty when none is configured
func GetMCPServers(ctx context.Context) (map[string]any, error) { return getMCPServers(ctx, nil) }
