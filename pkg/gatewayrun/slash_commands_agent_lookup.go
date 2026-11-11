package gatewayrun

// Live running agent of the session key, else its cached one, nil when neither is held
func (r *GatewayRunner) ResidentAgentFor(sessionKey string) any {
	if found := r.residentAgentFor(sessionKey); found != nil {
		return found
	}
	return nil
}

// Cached agent of the session key peeked under the cache lock without evicting it, nil when absent
func (r *GatewayRunner) CachedAgentFor(sessionKey string) any {
	if found := r.cachedAgentFor(sessionKey, false); found != nil {
		return found
	}
	return nil
}
