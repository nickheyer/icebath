package mcp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// One discovery pass connecting the allowed servers and returning the MCP tool names
type DiscoveryPass func(ctx context.Context, env *tools.Env, allowed []string) ([]string, error)

// Replaces the pass background discovery runs and returns the pass it replaced
func SwapDiscoveryPass(pass DiscoveryPass) DiscoveryPass {
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	previous := discoverMCPTools
	discoverMCPTools = pass
	return previous
}
