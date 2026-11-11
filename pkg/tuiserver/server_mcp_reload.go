package tuiserver

import (
	"context"
	"fmt"
	"strings"
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
			logger().Warn(fmt.Sprintf("Failed to persist mcp_reload_confirm=false: %s", panicMessage(failure)))
		}
	}
	out := map[string]any{"status": "reloaded", "loaded_rev": mcpReloadLoadedRev}
	if coalesced {
		out["coalesced"] = true
	}
	return ok(rid, out)
}

// Hints of the argv heads that must not run headless in the gateway process
var cliExecBlockedHeads = map[string]string{
	"setup":   "`icebath setup` needs a full terminal — run it outside the TUI",
	"gateway": "`icebath gateway` is long-running — run it in another terminal",
}

// Hints of the two word argv heads that must not run headless in the gateway process
var cliExecBlockedPairs = map[[2]string]string{
	{"sessions", "browse"}: "`icebath sessions browse` is interactive — use /resume here, or run browse in another terminal",
	{"config", "edit"}:     "`icebath config edit` needs $EDITOR in a real terminal",
}

// User hint of an argv that must not run headless, empty when it may run
func cliExecBlocked(argv []string) string {
	if len(argv) == 0 {
		return "bare `icebath` is interactive — use `/icebath chat -q …` or run `icebath` in another terminal"
	}
	if hint := cliExecBlockedHeads[strings.ToLower(argv[0])]; hint != "" {
		return hint
	}
	if len(argv) >= 2 {
		return cliExecBlockedPairs[[2]string{strings.ToLower(argv[0]), strings.ToLower(argv[1])}]
	}
	return ""
}
