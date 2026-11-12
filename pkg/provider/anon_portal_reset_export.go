package provider

import "context"

// Clears the guest mint memo, the free tier bootstrap, the challenge state and the resolved token memo
func ResetAnonPortalMemosForTests() {
	resetMintMemoForTests()
	resetFreeTierBootstrapForTests()
	resetChallengeForTests()
	resolveTokenCacheMu.Lock()
	resolveTokenCache = map[string]resolvedNousToken{}
	resolveTokenCacheMu.Unlock()
}

// Replaces the nous state reader, the returned function restoring it
func SwapCurrentNousStateForTests(read func(env PoolEnv) map[string]any) func() {
	prev := currentNousState
	currentNousState = read
	return func() { currentNousState = prev }
}

// Replaces the nous identity check of the claim blocker, the returned function restoring it
func SwapWelcomeHasNousIdentityForTests(has func(env PoolEnv) bool) func() {
	prev := welcomeHasNousIdentity
	welcomeHasNousIdentity = has
	return func() { welcomeHasNousIdentity = prev }
}

// Why the trial cannot be claimed here, the machine check skipped when assumed fresh
func ClaimBlocker(env PoolEnv, assumeFresh bool) (string, error) {
	return welcomeClaimBlocker(env, assumeFresh, nil)
}

// Forgets the setup record and the bootstrap state so the next boot runs again
func ResetFreeTierBootstrapForTests() { resetFreeTierBootstrapForTests() }

// Clears the guest mint memo alone, the next mint asking the portal again
func ResetMintMemoForTests() { resetMintMemoForTests() }

// Replaces the machine marker writer of the welcome claim, the returned function restoring it
func SwapWelcomeMarkUsedForTests(mark func(ctx context.Context) error) func() {
	prev := welcomeMarkUsed
	welcomeMarkUsed = mark
	return func() { welcomeMarkUsed = prev }
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

// Routes every ladder walk of the welcome trial checks through wrap, the returned function restoring the walk
func WrapWelcomeResolveForTests(wrap func(next func() (string, error)) (string, error)) func() {
	prev := welcomeResolveSkipped
	welcomeResolveSkipped = func(env PoolEnv, requested string) (string, error) {
		return wrap(func() (string, error) { return prev(env, requested) })
	}
	return func() { welcomeResolveSkipped = prev }
}

// Writes the Nous state every profile of the root shares, renewal keeping the recorded identity
func WriteSharedNousState(env PoolEnv, state map[string]any, renewal bool) {
	writeSharedNousState(env, state, renewal)
}
