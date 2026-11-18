package gateway

import "github.com/nickheyer/icebath/pkg/tuihost"

// Fans a session less event to every connected client of this process
func BroadcastGlobalEvent(name string, payload map[string]any) {
	tuihost.BroadcastGlobalEvent(name, payload)
}
