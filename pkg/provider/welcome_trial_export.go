package provider

// Something besides the welcome trial carries inference, asked of the resolver with the free tier hidden
func WelcomeProviderConfigured(env PoolEnv) (bool, error) { return welcomeProviderConfigured(env) }

// The profile's Nous identity is a guest whose claimed welcome trial carries inference here
func FreeInferenceActive(env PoolEnv) bool { return profileFreeInferenceActive(env) }

// A first chat can run here through the claimed or claimable trial or a configured provider
func WelcomeInferenceAvailable(env PoolEnv, assumeFresh bool) (bool, error) {
	return welcomeInferenceAvailable(env, assumeFresh)
}

// Path of the shared Nous store every profile of the root reads
func NousSharedStorePath(env PoolEnv) string { return nousSharedStorePath(env) }

// Why the welcome trial cannot be claimed here, empty when it can
func WelcomeClaimBlocker(env PoolEnv) (string, error) { return welcomeClaimBlocker(env, false, nil) }

// Marks the availability notice shown on the free tier identity, false when there is none to mark
func MarkGuestNoticeShown(env PoolEnv) (bool, error) { return markGuestNoticeShown(env) }

// Latest guest mint failure payload of the profile, nil when none is recorded
func LastMintFailurePayload(env PoolEnv) map[string]any { return lastMintFailure(env) }
