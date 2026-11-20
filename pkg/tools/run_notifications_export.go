package tools

import "github.com/nickheyer/icebath/pkg/config"

// Whether a process event surfaces as a notification under the config, subagent events only when enabled
func ShouldSurfaceNotification(cfg config.Tree, evt map[string]any) bool {
	return shouldSurfaceNotification(&Env{Config: cfg}, evt, nil)
}
