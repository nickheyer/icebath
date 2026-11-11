package gateway

import "maps"

// Copy of the stops latched for a turn that adopted no agent yet, by session id
func (a *SessionAuthority) PendingStops() map[string]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.pendingStops)
}
