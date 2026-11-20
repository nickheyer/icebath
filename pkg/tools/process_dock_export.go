package tools

// Fields of a process the live work dock paints, copied under the session lock
type ProcessDockFields struct {
	ID           string
	Command      string
	StartedAt    float64
	ExitedAt     float64
	ExitCode     *int
	OutputBuffer string
}

// Snapshot of the dock fields read under the session lock
func (s *ProcessSession) DockFields() ProcessDockFields {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := ProcessDockFields{ID: s.ID, Command: s.Command, StartedAt: s.StartedAt, ExitedAt: s.ExitedAt, OutputBuffer: s.OutputBuffer}
	if s.ExitCode != nil {
		code := *s.ExitCode
		f.ExitCode = &code
	}
	return f
}

// Drops a finished process from the registry so later listings no longer retain it
func (r *ProcessRegistry) ForgetFinished(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.finished, id)
}
