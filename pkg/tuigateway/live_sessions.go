package tuigateway

import "context"

// One live session id with its record
type LiveSession struct {
	SID     string
	Session *Session
}

// Live sessions in registration order, snapshotted under the registry lock
func LiveSessions() []LiveSession {
	entries := sessionEntries()
	out := make([]LiveSession, len(entries))
	for i, entry := range entries {
		out[i] = LiveSession{SID: entry.sid, Session: entry.session}
	}
	return out
}

// Effective user config of the active profile without the defaults merge, empty on any failure
func LoadCfg(ctx context.Context) map[string]any { return loadCfg(ctx) }

// Removes a slot and returns the value it held
func (s *Session) Pop(key string) (any, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.fields[key]
	delete(s.fields, key)
	return value, ok
}

// Stores every given slot under one lock
func (s *Session) Update(fields map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, value := range fields {
		s.fields[key] = value
	}
}

// Value of a slot, storing the default first when the record lacks it
func (s *Session) SetDefault(key string, def any) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value, ok := s.fields[key]; ok {
		return value
	}
	s.fields[key] = def
	return def
}

// Copy of every slot the record holds
func (s *Session) Fields() map[string]any {
	out := map[string]any{}
	if s == nil {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for key, value := range s.fields {
		out[key] = value
	}
	return out
}
