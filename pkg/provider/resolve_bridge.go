package provider

// Auth resolve_provider for a pool env
func resolveProvider(env PoolEnv, requested string) (string, error) {
	return authResolver(env).resolveProvider(requested, "", "")
}

// Canonical provider id auth resolution picks for a requested provider name
func ResolveAuthProvider(env PoolEnv, requested string) (string, error) {
	return resolveProvider(env, requested)
}
