package gateway

import "context"

// Appends a newly live plugin's MCP tools to the open chats of a home and queues its note
func RefreshPluginSessions(ctx context.Context, home, pluginKey, note string) error {
	var pluginNote *[2]string
	if note != "" {
		pluginNote = &[2]string{pluginKey, note}
	}
	return refreshLiveSessions(ctx, &home, true, pluginNote)
}
