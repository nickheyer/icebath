package gateway

import "context"

// Loads the index from disk once
func (s *SessionStore) ensureLoaded(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked(ctx)
}
