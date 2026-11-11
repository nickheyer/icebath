package gateway

import "context"

// Persists the session index under the store lock
func (s *SessionStore) Save(ctx context.Context) error { return s.save(ctx) }
