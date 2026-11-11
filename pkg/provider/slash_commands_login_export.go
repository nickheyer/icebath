package provider

// Nous provider entry of the auth store for the gateway login command, nil when absent
func CurrentNousState(env PoolEnv) map[string]any { return currentNousState(env) }

// Whether the Nous state is the anonymous free tier guest
func IsGuestState(state map[string]any) bool { return isGuestState(state) }
