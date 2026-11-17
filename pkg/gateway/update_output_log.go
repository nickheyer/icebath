package gateway

import "io"

// Update log handle behind the mirroring stream, nil when the run opened none
func (s *UpdateOutputStream) Log() io.Writer { return s.log }
