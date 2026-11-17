package provider

// Clears the guest mint memo, the free tier bootstrap, the challenge state and the resolved token memo
func ResetAnonPortalMemosForTests() {
	resetMintMemoForTests()
	resetFreeTierBootstrapForTests()
	resetChallengeForTests()
	resolveTokenCacheMu.Lock()
	resolveTokenCache = map[string]resolvedNousToken{}
	resolveTokenCacheMu.Unlock()
}

// Records a pending browser challenge for the env home the way a refused token exchange does
func RecordChallengeForTests(env PoolEnv, challenge BrowserChallenge, newAttempt bool) BrowserChallenge {
	return recordChallenge(env, challenge, newAttempt)
}

// Drops the pending browser challenge of the env home
func ClearPendingChallengeForTests(env PoolEnv) { clearPendingChallenge(env) }

// Pins the sign in offer clock to now seconds, the returned function restoring the wall clock
func PinFreeTierOfferClockForTests(now float64) func() {
	prev := freeTierOfferClock
	freeTierOfferClock = func() float64 { return now }
	return func() { freeTierOfferClock = prev }
}
