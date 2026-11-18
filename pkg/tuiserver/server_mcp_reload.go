package tuiserver

import (
	"context"
	"fmt"
	"sync"
)

// Persists one dot separated key into the active home config, the cli save_config_value assigned in cmd/icebath/wiring.go
var SaveConfigValue func(ctx context.Context, keyPath string, value any) bool

// Serialized reload state, the completed generation count and the mcp revision the last reload loaded
var (
	mcpReloadLock      sync.Mutex
	mcpReloadGen       int
	mcpReloadLoadedRev string
)

// Passes the reload leader re-hashes the config before accepting the loaded revision
const mcpReloadMaxPasses = 3

// Persists the always opt out when asked and answers the reloaded payload with its loaded revision
func finishReload(ctx context.Context, rid any, params map[string]any, coalesced bool) map[string]any {
	if truthy(params["always"]) {
		if failure := catchPanic(func() { SaveConfigValue(ctx, "approvals.mcp_reload_confirm", false) }); failure != nil {
			pkgLog().Warn(fmt.Sprintf("Failed to persist mcp_reload_confirm=false: %s", panicMessage(failure)))
		}
	}
	out := map[string]any{"status": "reloaded", "loaded_rev": mcpReloadLoadedRev}
	if coalesced {
		out["coalesced"] = true
	}
	return ok(rid, out)
}
