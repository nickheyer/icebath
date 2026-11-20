package skills

// Whether the kanban playbook is relevant for this scope, for callers outside the package
func (s *Scope) DetectKanban() bool { return s.detectKanban() }
