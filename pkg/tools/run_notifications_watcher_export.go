package tools

// Fields of a background process the gateway watcher reads, copied under the session lock
type ProcessWatchFields struct {
	Command           string
	TaskID            string
	OwnerTaskID       string
	ParentSessionID   string
	StartedAt         float64
	Exited            bool
	ExitCode          *int
	CompletionReason  string
	TerminationSource string
	OutputBuffer      string
}

// Snapshot of the watcher fields read under the session lock
func (s *ProcessSession) WatchFields() ProcessWatchFields {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := ProcessWatchFields{Command: s.Command, TaskID: s.TaskID, OwnerTaskID: s.OwnerTaskID, ParentSessionID: s.ParentSessionID,
		StartedAt: s.StartedAt, Exited: s.Exited, CompletionReason: s.CompletionReason, TerminationSource: s.TerminationSource,
		OutputBuffer: s.OutputBuffer}
	if s.ExitCode != nil {
		code := *s.ExitCode
		f.ExitCode = &code
	}
	return f
}

// Background process output through the plugin transform seam, nil returncode while the process runs
func TransformProcessOutput(env *Env, output, command string, returncode *int, taskID string) string {
	return transformProcessOutput(env, output, command, returncode, taskID)
}
