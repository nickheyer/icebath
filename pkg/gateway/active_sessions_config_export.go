package gateway

// Configured active session cap as the registry resolver reads it, nil when unset
func (c *GatewayConfig) MaxConcurrentSessionsAttr() any {
	if c == nil || c.MaxConcurrentSessions <= 0 {
		return nil
	}
	return c.MaxConcurrentSessions
}
