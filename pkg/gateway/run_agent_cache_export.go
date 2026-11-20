package gateway

import "context"

// Whether the Slack tools are loaded for the session context renderer gate
func SlackToolsLoaded(ctx context.Context) bool { return slackToolsLoaded(ctx) }

// Profile home that owns a session key, empty for the default profile
func (s *SessionStore) ProfileHomeForKey(sessionKey string) string {
	return s.profileHomeForKey(sessionKey)
}

// Interrupt event of a session's active processing guard, nil when the session holds none
func (b *BasePlatformAdapter) ActiveSessionEvent(sessionKey string) *InterruptEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeSessions[sessionKey]
}

// Parks an event in a session's pending slot for the adapter's post command drain
func (b *BasePlatformAdapter) SetPendingMessage(sessionKey string, event *MessageEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pendingMessages == nil {
		b.pendingMessages = map[string]*MessageEvent{}
	}
	b.pendingMessages[sessionKey] = event
}
