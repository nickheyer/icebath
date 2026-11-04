package provider

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
)

// Process slot for one catalog cache that lives per home key under a home override
type profileSlot[T any] struct {
	mu        sync.Mutex
	module    T
	moduleSet bool
	byHome    map[string]T
}

func newProfileSlot[T any]() *profileSlot[T] { return &profileSlot[T]{byHome: map[string]T{}} }

// Value for the active profile, false when the slot is cold for it
func (s *profileSlot[T]) get(ctx context.Context) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if home.Override(ctx) == "" {
		return s.module, s.moduleSet
	}
	v, ok := s.byHome[home.Key(home.Dir(ctx))]
	return v, ok
}

func (s *profileSlot[T]) set(ctx context.Context, value T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if home.Override(ctx) == "" {
		s.module, s.moduleSet = value, true
		return
	}
	s.byHome[home.Key(home.Dir(ctx))] = value
}

// Clears the active profile's value so the next read starts cold
func (s *profileSlot[T]) clear(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if home.Override(ctx) == "" {
		var zero T
		s.module, s.moduleSet = zero, false
		return
	}
	delete(s.byHome, home.Key(home.Dir(ctx)))
}

// Drops every profile's value
func (s *profileSlot[T]) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero T
	s.module, s.moduleSet = zero, false
	s.byHome = map[string]T{}
}
