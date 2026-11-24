package tools

import (
	"context"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Whether a process event surfaces as a notification in the profile ctx serves, subagent events only when enabled
func ShouldSurfaceNotification(ctx context.Context, evt map[string]any) bool {
	surface := settings.DelegationSurfaceChildProcessNotifications.Get(ctx)
	return shouldSurfaceNotification(nil, evt, &surface)
}
