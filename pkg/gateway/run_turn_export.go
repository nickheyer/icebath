package gateway

// Adapter the stream consumer sends and edits through
func (s *GatewayStreamConsumer) Adapter() PlatformAdapter { return s.adapter }

// Forgets the streamed audio marker of a finished turn so the next turn of the session is not suppressed
func DiscardStreamingTTSCompletedTurn(adapter PlatformAdapter, sessionKey string, runGeneration int) {
	b := adapter.Base()
	key, ok := b.streamingTTSTurnKey(sessionKey, runGeneration, nil)
	if !ok {
		return
	}
	b.mu.Lock()
	delete(b.streamingTTSCompletedTurns, key)
	b.mu.Unlock()
}
