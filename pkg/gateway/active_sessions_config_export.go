package gateway

// Configured active session cap as the registry resolver reads it, nil when unset
func (c *GatewayConfig) MaxConcurrentSessionsAttr() any {
	if c == nil || c.MaxConcurrentSessions == nil {
		return nil
	}
	return c.MaxConcurrentSessions
}
