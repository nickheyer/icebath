package provider

// Auth resolve_provider for a pool env, the free tier rungs included
func resolveProvider(env PoolEnv, requested string) (string, error) {
	return authResolver(env).resolveProvider(requested, "", "", false)
}

// Auth resolve_provider with skip_free_tier set, so a guest identity never answers
func resolveProviderSkippingFreeTier(env PoolEnv, requested string) (string, error) {
	return authResolver(env).resolveProvider(requested, "", "", true)
}
