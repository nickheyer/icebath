package provider

// The profile's Nous identity is a guest whose claimed welcome trial carries inference here
func FreeInferenceActive(env PoolEnv) bool { return profileFreeInferenceActive(env) }

// Marks the availability notice shown on the free tier identity, false when there is none to mark
func MarkGuestNoticeShown(env PoolEnv) (bool, error) { return markGuestNoticeShown(env) }

// Latest guest mint failure payload of the profile, nil when none is recorded
func LastMintFailurePayload(env PoolEnv) map[string]any { return lastMintFailure(env) }
