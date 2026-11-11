package gateway

import "context"

// Tool snapshot refresh of the live TUI server sessions of a home, assigned by the command wiring
var RefreshLiveSessions func(ctx context.Context, home *string, preservePrefix bool, pluginNote *[2]string) error

// Appends a newly live plugin's MCP tools to the open chats of a home and queues its note
func RefreshPluginSessions(ctx context.Context, home, pluginKey, note string) error {
	var pluginNote *[2]string
	if note != "" {
		pluginNote = &[2]string{pluginKey, note}
	}
	return RefreshLiveSessions(ctx, &home, true, pluginNote)
}
