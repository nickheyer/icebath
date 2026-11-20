package browser

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Closes every live browser session so the next tool call picks up the current backend
func CleanupAllBrowsers(ctx context.Context, env *tools.Env) {
	cleanupAllBrowsers(tools.WithEnvHome(ctx, env), env)
}
