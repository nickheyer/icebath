package state

// Windows has no POSIX advisory locks for a stray close to cancel, so nothing is guarded
func (e *Engine) guardHold(_ string, held guardHeld) guardHeld {
	if held == nil {
		held = guardHeld{}
	}
	return held
}

// Windows guards nothing, so a release only forgets the claim
func (e *Engine) guardRelease(held guardHeld) { clear(held) }
