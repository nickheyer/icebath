package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Resumes a stored session through the request handler with its messages omitted
func kanbanResume(sessionID string) map[string]any {
	return handleRequest(context.Background(), map[string]any{"id": "r-" + sessionID, "method": "session.resume",
		"params": map[string]any{"session_id": sessionID, "omit_messages": true}})
}

// Whether any live session holds the stored key
func liveForKey(key string) bool {
	for _, live := range tuihost.LiveSessions() {
		if live.Session.Get("session_key") == key {
			return true
		}
	}
	return false
}
