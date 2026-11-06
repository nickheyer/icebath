package observability

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/tools"
)

// ObserveLifecycle dispatches a lifecycle event to the built in observability features
func ObserveLifecycle(ctx context.Context, env *tools.Env, hookName string, kwargs map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn(fmt.Sprintf("Built-in observability hook failed: %s", hookName), "error", fmt.Sprint(r))
		}
	}()
	relayObserveLifecycle(ctx, env, hookName, kwargs)
}

// HandlesHook reports whether any built in observability feature handles a hook
func HandlesHook(ctx context.Context, hookName string) bool { return relayHandlesHook(ctx, hookName) }
