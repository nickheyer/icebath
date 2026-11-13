package backend

// Cached primary connection future a dial would join, nil when none is cached
func (s *Service) PrimaryConnection() *Future[Connection] { return s.state.Promise() }

// Pooled backends with their remote base URLs and connection futures
func (s *Service) PoolBackends() []PoolBackend { return s.poolBackends() }

// Drops one pooled backend from the pool
func (s *Service) StopPoolBackend(key string) error { return s.stopPoolBackend(key) }

// Redials a retired pool key through the claim guarded ensure path a renderer dial takes
func (s *Service) RedialPoolBackend(poolKey string) (Connection, error) {
	route := ParseBackendScopeKey(poolKey)
	return RunDial(&s.claims, poolKey, func() (Connection, error) {
		if route.ConnectionID != nil && *route.ConnectionID != "" {
			return s.routes.EnsureRegistryBackend(*route.ConnectionID, route.Profile)
		}
		return s.EnsureBackend(route.Profile, EnsureOptions{})
	})
}
