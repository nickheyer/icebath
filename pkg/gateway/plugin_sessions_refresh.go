package gateway

import "context"

// Tool snapshot refresh of the open chats of a home, installed by the gateway runner that serves them, nil while no runner serves chats in this process
var RefreshLiveSessions func(ctx context.Context, home *string, preservePrefix bool, pluginNote *[2]string) error

// Appends a newly live plugin's MCP tools to the open chats of a home and queues its note, nothing to do in a process that serves no chats
func RefreshPluginSessions(ctx context.Context, home, pluginKey, note string) error {
	if RefreshLiveSessions == nil {
		return nil
	}
	var pluginNote *[2]string
	if note != "" {
		pluginNote = &[2]string{pluginKey, note}
	}
	return RefreshLiveSessions(ctx, &home, true, pluginNote)
}
