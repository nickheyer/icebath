package tuigateway

import "sync"

// Live session record, a dict of named slots that readers and writers on any goroutine share
type Session struct {
	mu     sync.RWMutex
	fields map[string]any
}

// Record holding a copy of the given slots
func NewSession(fields map[string]any) *Session {
	s := &Session{fields: make(map[string]any, len(fields))}
	for key, value := range fields {
		s.fields[key] = value
	}
	return s
}

// Value of a slot, nil when the record or the slot is absent
func (s *Session) Get(key string) any {
	value, _ := s.Lookup(key)
	return value
}

// Value of a slot and whether the record holds it
func (s *Session) Lookup(key string) (any, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.fields[key]
	return value, ok
}

// Stores a slot
func (s *Session) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fields[key] = value
}

// Whether the record is absent or holds no slot
func (s *Session) empty() bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.fields) == 0
}
