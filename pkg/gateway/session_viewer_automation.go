package gateway

import "github.com/nickheyer/icebath/pkg/internal/pyrepr"

// Whether a local viewer drives the session's goal and loop judging, or its heartbeat ticks for viewer_heartbeat
func ViewerDriven(authority *SessionAuthority, sessionID, registry string) bool {
	if authority == nil {
		return false
	}
	authority.mu.Lock()
	var connection *AuthorityConnection
	switch registry {
	case "viewer_heartbeat":
		connection = authority.viewerHeartbeat[sessionID]
	default:
		connection = authority.viewerAutomation[sessionID]
	}
	authority.mu.Unlock()
	if connection == nil {
		return false
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return pyrepr.Truthy(connection.subscriptions[sessionID])
}
