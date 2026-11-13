package backend

// Re-points the main window route at the applied primary of registry, then announces the apply
func (s *Service) ApplyPrimaryRoute(registry ConnectionRegistry) {
	if id := s.host.MainWindowID(); id != "" {
		profile := s.primaryProfileKey()
		if previous := s.windowRoutes.Get(id); previous != nil && previous.Profile != nil {
			profile = *previous.Profile
		}
		route := AppliedPrimaryWindowRoute(registry, profile)
		value := map[string]any{"profile": *route.Profile, "registryScoped": route.RegistryScoped}
		if route.ConnectionID != nil {
			value["connectionId"] = *route.ConnectionID
		}
		s.SetActiveConnectionRoute(id, value)
	}
	s.SendConnectionApplied()
}

// Clears the local bootstrap failure latch a remote apply bypasses
func (s *Service) ClearBootstrapFailure() { s.runtime.SetBootstrapFailure(nil) }
