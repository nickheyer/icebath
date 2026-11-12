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

// Pins the sign in offer clock to now seconds, the returned function restoring the wall clock
func PinFreeTierOfferClockForTests(now float64) func() {
	prev := freeTierOfferClock
	freeTierOfferClock = func() float64 { return now }
	return func() { freeTierOfferClock = prev }
}
