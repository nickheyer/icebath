package gateway

// Routing key the durable active turn marker was written under, nil when the event holds no marker
func (e *MessageEvent) GatewayActiveTurnSessionKey() *string {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	return e.activeTurnSessionKey
}

// Stamps the durable active turn marker key and token the runner owns for this event
func (e *MessageEvent) SetGatewayActiveTurnMarker(sessionKey string, token any) {
	eventTurnStateMu.Lock()
	e.activeTurnSessionKey, e.activeTurnToken = &sessionKey, token
	eventTurnStateMu.Unlock()
}

// Drops both durable active turn marker attributes from the event
func (e *MessageEvent) ClearGatewayActiveTurnMarker() {
	eventTurnStateMu.Lock()
	e.activeTurnSessionKey, e.activeTurnToken = nil, nil
	eventTurnStateMu.Unlock()
}

// Refusal a local turn answers with after its context references were refused, nil when none
func (e *MessageEvent) GatewayTurnRefusal() *string {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	return e.turnRefusal
}

// Records the refusal a refused local turn replies with
func (e *MessageEvent) SetGatewayTurnRefusal(refusal string) {
	eventTurnStateMu.Lock()
	e.turnRefusal = &refusal
	eventTurnStateMu.Unlock()
}

// Display kind view of a message event
type messageDisplayEvent struct{ e *MessageEvent }

func (d messageDisplayEvent) IsInternal() bool              { return d.e.Internal }
func (d messageDisplayEvent) EventMetadata() map[string]any { return d.e.Metadata }

// Heartbeat session id the poller stamped, None when absent
func (d messageDisplayEvent) HeartbeatSessionID() any {
	if id := d.e.HeartbeatSessionID(); id != nil {
		return *id
	}
	return nil
}

// Message id of the event, None when absent
func (d messageDisplayEvent) EventMessageID() any {
	if d.e.MessageID != nil {
		return *d.e.MessageID
	}
	return nil
}

// Display kind view the response filters read off a message event, nil for a nil event
func DisplayEventOf(e *MessageEvent) DisplayEvent {
	if e == nil {
		return nil
	}
	return messageDisplayEvent{e: e}
}

// Records the text streaming already delivered for the loop and goal hooks that read the turn result
func (e *MessageEvent) SetStreamedFinalResponse(text string) {
	eventTurnStateMu.Lock()
	e.streamedFinalResponse = &text
	eventTurnStateMu.Unlock()
}

// Text streaming already delivered for this turn, nil when the reply was not streamed
func (e *MessageEvent) StreamedFinalResponse() *string {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	return e.streamedFinalResponse
}

// Records whether a queued chain's terminal notification reply is muted
func (e *MessageEvent) SetNotificationReplyMuted(muted bool) { e.notificationReplyMuted = &muted }
