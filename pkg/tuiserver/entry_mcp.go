package tuiserver

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/mcp"
)

// Set once any profile of this process has configured MCP servers, gating the per build wait
var mcpDiscoveryEnabled atomic.Bool

// Shared discovery start of the entry, the real function swapped only where a hermes case installs a fake
var entryStartBackgroundDiscovery = mcp.StartBackgroundDiscovery

// Starts the shared background discovery of the caller's profile home, logging a failure at the given level
func spawnMCPDiscovery(ctx context.Context, level, message string) {
	defer func() {
		if r := recover(); r != nil {
			if level == "warning" {
				entryLogger().Warn(message, "error", r)
			} else {
				entryLogger().Debug(message, "error", r)
			}
		}
	}()
	entryStartBackgroundDiscovery(ctx, configEnv(ctx), entryLogger(), "tui-mcp-discovery")
}

// Retries the idempotent spawn of the caller's profile then waits for its discovery up to the resolved bound
func waitForEntryMCPDiscovery(ctx context.Context, timeout *time.Duration) {
	if !mcpDiscoveryEnabled.Load() {
		return
	}
	spawnMCPDiscovery(ctx, "debug", "TUI MCP discovery retry-spawn failed")
	suppress(func() { mcp.WaitForDiscovery(ctx, configEnv(ctx), timeout, false) })
}

// Whether a background MCP discovery of the caller's profile is still running
func mcpDiscoveryInFlight(ctx context.Context) (inFlight bool) {
	defer func() {
		if recover() != nil {
			inFlight = false
		}
	}()
	return mcp.DiscoveryInFlight(ctx)
}

// Joins the caller's discovery, true once none is alive
func joinMCPDiscovery(ctx context.Context, timeout *time.Duration) (done bool) {
	defer func() {
		if recover() != nil {
			done = true
		}
	}()
	return mcp.JoinDiscovery(ctx, timeout)
}

// Starts background MCP discovery for the current profile context when it configures any server
func ensureMCPDiscoveryStarted(ctx context.Context) {
	if !mcp.HasConfiguredServers(ctx) {
		return
	}
	mcpDiscoveryEnabled.Store(true)
	spawnMCPDiscovery(ctx, "warning", "Background MCP tool discovery failed to start")
}
