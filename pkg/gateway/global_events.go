package gateway

import "github.com/nickheyer/icebath/pkg/tuigateway"

// Fans a session less event to every connected client of this process
func BroadcastGlobalEvent(name string, payload map[string]any) {
	tuigateway.BroadcastGlobalEvent(name, payload)
}
