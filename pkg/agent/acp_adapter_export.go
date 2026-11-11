package agent

// Replaces the thinking status callback, nil silencing it
func (a *AIAgent) SetThinkingCallback(cb any) { a.thinkingCallback = cb }

// Records the delegation backend the tool snapshot is built from
func (a *AIAgent) SetDelegationBackend(backend string) { a.setDelegationBackend(backend) }

// Clears the session scoped token, cost and compressor state for a fresh session
func (a *AIAgent) ResetSessionState() { a.resetSessionState(nil, "", false) }

// False when automatic context compaction is turned off for this agent
func (a *AIAgent) CompressionEnabled() bool { return a.compressionEnabled }

// Runs fn with the session store detached so a compression inside it splits no session, then restores the store
func (a *AIAgent) WithSessionStoreDetached(fn func()) {
	saved := a.sessionDB
	a.sessionDB = nil
	defer func() { a.sessionDB = saved }()
	fn()
}
