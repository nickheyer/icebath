package backend

// ForgetLocalGatewayDescriptor drops the cached descriptor of a stale canonical local gateway so the next ensure re-runs
func (s *Service) ForgetLocalGatewayDescriptor(profile string) error {
	return s.forgetLocalGatewayDescriptor(profile)
}

// PreparePluginDial reserves a one use plugin socket URL bound to the window
func (s *Service) PreparePluginDial(origin, path, ticket, windowID string) (string, error) {
	return s.dials.PreparePlugin(origin, path, ticket, windowID)
}

// RunDialClaim coalesces concurrent dials of one backend scope onto a single dial
func (s *Service) RunDialClaim(key string, dial func() (Connection, error)) (Connection, error) {
	return RunDial(&s.claims, key, dial)
}

// ForgetPoolEntry drops a pooled descriptor without stopping its runtime, false when the key held none
func (s *Service) ForgetPoolEntry(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pool[key]
	delete(s.pool, key)
	return ok
}
