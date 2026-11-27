package mcp

import "github.com/nickheyer/icebath/internal/mcp"

func Register(host *mcp.MCP) *mcp.Registry {
	registry := new(mcp.Registry)
	host.Register(registry)
	return registry
}
