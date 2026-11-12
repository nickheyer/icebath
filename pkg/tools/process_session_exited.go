package tools

// Whether the process recorded its exit, read under the session lock
func (s *ProcessSession) IsExited() bool { return s.isExited() }
