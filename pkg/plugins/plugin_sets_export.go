package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
)

// Allow list of enabled plugin keys of the active scope home, empty on a failed read
func EnabledPluginSet(ctx context.Context) map[string]bool { return getEnabledSet(home.Dir(ctx)) }

// Deny list of disabled plugin keys of the active scope home, empty on a failed read
func DisabledPluginSet(ctx context.Context) map[string]bool { return getDisabledSet(home.Dir(ctx)) }

// Selects the memory provider of the active scope home in config.yaml
func SaveMemoryProvider(ctx context.Context, name string) error {
	return saveMemoryProvider(ctx, home.Dir(ctx), name)
}

// Selects the context engine of the active scope home in config.yaml
func SaveContextEngine(ctx context.Context, name string) error {
	return saveContextEngine(ctx, home.Dir(ctx), name)
}
