package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
)

// Allow list of enabled plugin keys of the active scope home, empty on a failed read
func EnabledPluginSet(ctx context.Context) map[string]bool { return getEnabledSet(home.Dir(ctx)) }

// Deny list of disabled plugin keys of the active scope home, empty on a failed read
func DisabledPluginSet(ctx context.Context) map[string]bool { return getDisabledSet(home.Dir(ctx)) }
