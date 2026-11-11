package gateway

import "context"

// Session id the heartbeat owner resolved to before the agent runner starts, nil before resolution
func (e *MessageEvent) HeartbeatResolvedSessionID() *string {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	return e.heartbeatResolvedID
}

// Records the session id the heartbeat owner resolved to
func (e *MessageEvent) SetHeartbeatResolvedSessionID(id *string) {
	eventTurnStateMu.Lock()
	e.heartbeatResolvedID = id
	eventTurnStateMu.Unlock()
}

// Whether the heartbeat attempt entered the agent runner
func (e *MessageEvent) HeartbeatExecutionStarted() bool {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	return e.heartbeatStarted
}

// Records whether the heartbeat attempt entered the agent runner
func (e *MessageEvent) SetHeartbeatExecutionStarted(started bool) {
	eventTurnStateMu.Lock()
	e.heartbeatStarted = started
	eventTurnStateMu.Unlock()
}

// Session database a routing key resolves to
func (s *SessionStore) DBForKey(ctx context.Context, sessionKey string) SessionStoreDB {
	return s.dbForKey(ctx, sessionKey)
}
