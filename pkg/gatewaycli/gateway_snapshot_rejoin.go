package gatewaycli

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Whether a host control answer confirms name under key without an error
func AckConfirmed(answer *pyjson.Object, key, name string) bool {
	return ackConfirmed(answer, key, name)
}

// Why a host control answer did not confirm the operation
func AckFailure(answer *pyjson.Object) string { return ackFailure(answer) }

// Forgets every session snapshot this viewer adopted, the next attach resuming afresh
func (s *GatewaySessionStore) ForgetSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots = map[string]*pyjson.Object{}
	s.current = nil
}
