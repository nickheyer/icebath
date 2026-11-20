package gateway

import (
	"context"
)

// Re-send admission reply when the conversation FIFO is full
const QueueFullReply = queueFullReply

// Context binding a planned retry rewrite to the re-send admitted under it
func BindRetryRewrite(ctx context.Context, rewrite *RetryRewrite) context.Context {
	return bindRetryRewrite(ctx, rewrite)
}

// Publishes the live entry of a reserved route and names its lane
func (a *SessionAuthority) PublishRoute(ctx context.Context, source *SessionSource, sid, route string) (SessionRef, error) {
	return a.publishRoute(ctx, source, sid, route)
}

// Refusal while the runtime drains or retires, nil when admission is open
func (a *SessionAuthority) RequireAdmissionOpen() error {
	return a.requireAdmissionOpen(false)
}

// Database holding the session transcript, nil when none resolves
func (s *SessionStore) DBForSessionID(ctx context.Context, sessionID string) SessionStoreDB {
	return s.dbForSessionID(ctx, sessionID)
}

// Drops the dirty transcript mark of the session
func (s *SessionStore) ClearDirtyTranscript(sessionID string) {
	s.clearDirtyTranscript(sessionID)
}
