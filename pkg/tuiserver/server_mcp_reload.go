package tuiserver

import (
	"context"
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
