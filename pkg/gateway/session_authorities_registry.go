package gateway

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Registry of the session authorities one runtime serves keyed by the home key of each profile home
type SessionAuthorities struct {
	LaunchKey   string
	Multiplexed bool
	mu          sync.Mutex
	byKey       *pydict.Dict[string, *SessionAuthority]
	names       *pydict.Dict[string, *string]
	starting    *pydict.Dict[string, chan struct{}]
}

// Registry holding no slot yet for the launch home, multiplexed when the config says so
func NewSessionAuthorities(launchHome string, multiplexed bool) *SessionAuthorities {
	return &SessionAuthorities{
		LaunchKey:   home.Key(launchHome),
		Multiplexed: multiplexed,
		byKey:       pydict.New[string, *SessionAuthority](),
		names:       pydict.New[string, *string](),
		starting:    pydict.New[string, chan struct{}](),
	}
}

// Registers the authority of a home, a nil authority reserving the slot while it is built
func (s *SessionAuthorities) Add(profileHome string, authority *SessionAuthority, name *string) error {
	key := home.Key(profileHome)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byKey.Has(key) {
		return fmt.Errorf("duplicate session authority for %s", profileHome)
	}
	s.byKey.Set(key, authority)
	s.names.Set(key, name)
	if authority == nil {
		s.starting.Set(key, make(chan struct{}))
	}
	if key != s.LaunchKey {
		s.Multiplexed = true
	}
	return nil
}

// Fills the slot a nil add reserved once the authority exists
func (s *SessionAuthorities) Replace(profileHome string, authority *SessionAuthority) error {
	key := home.Key(profileHome)
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.byKey.Get(key); !ok || current != nil {
		return fmt.Errorf("no reserved session authority slot for %s", profileHome)
	}
	s.byKey.Set(key, authority)
	s.endStart(key)
	return nil
}

// Drops the slot of a home and returns its authority, refusing the launch slot
func (s *SessionAuthorities) Remove(profileHome string) (*SessionAuthority, error) {
	key := home.Key(profileHome)
	if key == s.LaunchKey {
		return nil, fmt.Errorf("the launch session authority cannot be removed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names.Delete(key)
	s.endStart(key)
	authority, _ := s.byKey.Get(key)
	s.byKey.Delete(key)
	return authority, nil
}

// Releases the waiters of a slot whose build ended, filled or dropped
func (s *SessionAuthorities) endStart(key string) {
	if started, ok := s.starting.Get(key); ok {
		s.starting.Delete(key)
		close(started)
	}
}

// Key and started channel of the slot the profile names while its authority is still being built
func (s *SessionAuthorities) Starting(ctx context.Context, profile string) (string, <-chan struct{}, bool) {
	s.mu.Lock()
	keys := s.starting.Keys()
	chans := s.starting.Values()
	s.mu.Unlock()
	for i, key := range keys {
		if profiles.ProfileMatchesHome(ctx, profile, key) {
			return key, chans[i], true
		}
	}
	return "", nil, false
}

// Keys of every slot whose authority is still being built
func (s *SessionAuthorities) StartingHomes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starting.Keys()
}

// Served profile name of the authority, false for the launch profile
func (s *SessionAuthorities) ProfileName(authority *SessionAuthority) (string, bool) {
	key := home.Key(authority.ProfileID)
	if key == s.LaunchKey {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.names.Get(key)
	if !ok || name == nil {
		return "", false
	}
	return *name, true
}

// Count of built authorities, a slot still being built not counted
func (s *SessionAuthorities) Len() int {
	return len(s.Authorities())
}

// Built authorities including retiring ones, a snapshot that skips slots still being built
func (s *SessionAuthorities) Authorities() []*SessionAuthority {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*SessionAuthority
	for _, authority := range s.byKey.Values() {
		if authority != nil {
			out = append(out, authority)
		}
	}
	return out
}

// Whether the home holds a slot, a slot still being built included
func (s *SessionAuthorities) Contains(profileHome string) bool {
	key := home.Key(profileHome)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byKey.Has(key)
}

// Authority of the launch home, KeyError when the launch slot was never added
func (s *SessionAuthorities) Launch() (*SessionAuthority, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	authority, ok := s.byKey.Get(s.LaunchKey)
	if !ok {
		return nil, &PyError{Kind: "KeyError", Msg: s.LaunchKey}
	}
	return authority, nil
}

// Authority owning the home, nil when this process does not serve it
func (s *SessionAuthorities) ForHome(profileHome string) *SessionAuthority {
	key := home.Key(profileHome)
	s.mu.Lock()
	defer s.mu.Unlock()
	authority, _ := s.byKey.Get(key)
	return authority
}

// Authority owning the home, profile_mismatch when this process does not serve it
func (s *SessionAuthorities) Require(profileHome string) (*SessionAuthority, error) {
	authority := s.ForHome(profileHome)
	if authority == nil {
		return nil, state.NewRuntimeStoreError("profile_mismatch", nil, nil)
	}
	return authority, nil
}

// Authority of the active runtime scope, the launch one unscoped or never multiplexed, nil for an unserved scope
func (s *SessionAuthorities) Active(ctx context.Context) (*SessionAuthority, error) {
	if !s.Multiplexed || home.Override(ctx) == "" {
		return s.Launch()
	}
	key := home.Key(home.Dir(ctx))
	s.mu.Lock()
	defer s.mu.Unlock()
	authority, _ := s.byKey.Get(key)
	return authority, nil
}

// Authorities accepting work, neither a slot still being built nor a retiring one
func (s *SessionAuthorities) Serving() []*SessionAuthority {
	var out []*SessionAuthority
	for _, authority := range s.Authorities() {
		if !authority.Retiring {
			out = append(out, authority)
		}
	}
	return out
}

// Profile id and home of each serving authority
func (s *SessionAuthorities) ServedProfiles() []map[string]any {
	out := []map[string]any{}
	for _, authority := range s.Serving() {
		out = append(out, map[string]any{"profile_id": authority.ProfileID, "home": authority.ProfileID})
	}
	return out
}

// Set of profile ids of the serving authorities
func (s *SessionAuthorities) ProfileIDs() map[string]bool {
	out := map[string]bool{}
	for _, authority := range s.Serving() {
		out[authority.ProfileID] = true
	}
	return out
}
