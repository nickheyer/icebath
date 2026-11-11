package tuigateway

import "github.com/nickheyer/icebath/pkg/internal/pyrepr"

// Record under the id, else the first registered record whose session key equals it, under one registry lock
func SessionRecordByIDOrKey(sessionID string) *Session {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	if record := sessions[sessionID]; record != nil {
		return record
	}
	for _, sid := range sessionOrder {
		candidate := sessions[sid]
		if candidate == nil {
			continue
		}
		key := candidate.Get("session_key")
		if !pyrepr.Truthy(key) {
			key = ""
		}
		if pyrepr.Str(key) == sessionID {
			return candidate
		}
	}
	return nil
}
