package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the idempotent plugin discovery web dispatch and the tools picker run before reading plugin registries
func init() {
	tools.EnsurePluginsDiscovered = func(ctx context.Context) error {
		ensurePluginsDiscovered(ctx, false)
		return nil
	}
}
