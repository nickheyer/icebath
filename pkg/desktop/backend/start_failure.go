package backend

// Latches the backend start failure every start returns until a reset, nil clearing it
func (s *Service) SetStartFailure(err error) {
	s.mu.Lock()
	s.startFailure = err
	s.mu.Unlock()
}

// Clears the latched start and reauth failures so the next start runs the full backend flow
func (s *Service) ClearStartFailures() {
	s.mu.Lock()
	s.startFailure = nil
	s.reauthFailure = nil
	s.mu.Unlock()
}

// Whether the primary backend process this app spawned is still running
func (s *Service) PrimaryProcessAlive() bool {
	pid, ok := s.state.Process()
	return ok && pid > 0 && IsPidAlive(pid)
}
