package provider

// Forgets every logged key as a test reset does
func (s *auxOnceSet) clear() {
	s.mu.Lock()
	s.seen = map[string]bool{}
	s.mu.Unlock()
}
