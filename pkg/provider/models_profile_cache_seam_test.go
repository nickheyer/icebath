package provider

// Drops every profile's value
func (s *profileSlot[T]) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero T
	s.module, s.moduleSet = zero, false
	s.byHome = map[string]T{}
}
