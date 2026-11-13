package gateway

import "context"

// Pinned database or the one of the active scope, a live system guard returned as the error
func (s *SessionStore) DB(ctx context.Context) (SessionStoreDB, error) {
	if db, ok := s.pinned(); ok {
		return db, nil
	}
	return s.openSessionDBForActiveScope(ctx, "")
}
