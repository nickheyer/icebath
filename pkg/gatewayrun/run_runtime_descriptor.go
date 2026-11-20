package gatewayrun

import (
	"maps"
	"sync"
	"sync/atomic"
)

// Runtime descriptor republished whole on every change, so a reader on another goroutine never meets a write
type runtimeDescriptorState struct {
	mu      sync.Mutex
	current atomic.Pointer[map[string]any]
}

// Descriptor as last published, nil before the runtime set one, never changed once returned
func (s *runtimeDescriptorState) load() map[string]any {
	if published := s.current.Load(); published != nil {
		return *published
	}
	return nil
}

// Publishes descriptor as the current one, the map belonging to the state from here on
func (s *runtimeDescriptorState) store(descriptor map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current.Store(&descriptor)
}

// Publishes a copy of the descriptor that edit changed, starting from an empty one when none was set
func (s *runtimeDescriptorState) update(edit func(descriptor map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[string]any{}
	if published := s.current.Load(); published != nil {
		next = maps.Clone(*published)
		if next == nil {
			next = map[string]any{}
		}
	}
	edit(next)
	s.current.Store(&next)
}

// Sets one descriptor field, the write a holder outside this package makes through the runner
func (r *GatewayRunner) SetRuntimeDescriptorField(key string, value any) {
	r.sessionRuntimeDescriptor.update(func(descriptor map[string]any) { descriptor[key] = value })
}

// Parked profiles as last published, nil when none was parked
func (r *GatewayRunner) parkedProfiles() map[string]any {
	parked, _ := r.sessionRuntimeDescriptor.load()["parked_profiles"].(map[string]any)
	return parked
}

// Publishes a copy of the parked profiles that edit changed and returns it
func (r *GatewayRunner) updateParkedProfiles(edit func(parked map[string]any)) map[string]any {
	var next map[string]any
	r.sessionRuntimeDescriptor.update(func(descriptor map[string]any) {
		next = map[string]any{}
		if parked, isMap := descriptor["parked_profiles"].(map[string]any); isMap {
			next = maps.Clone(parked)
		}
		edit(next)
		descriptor["parked_profiles"] = next
	})
	return next
}
