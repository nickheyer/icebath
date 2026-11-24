package tuiserver

import (
	"context"
)

// Persists one dot separated key into the active home config, the cli save_config_value assigned in cmd/icebath/wiring.go
var SaveConfigValue func(ctx context.Context, keyPath string, value any) bool
