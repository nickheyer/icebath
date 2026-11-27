package auxiliary

import (
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
)

// What calls learn as they go, kept while icebath runs: providers out of service for a while, and what each route turned down. One is kept
// by whatever makes the calls, for all of them
type State struct {
	mu      sync.Mutex
	outages map[string]outage
	refused map[string]bool
}

type outage struct {
	until  time.Time
	reason string
}

func NewState() *State {
	return &State{outages: map[string]outage{}, refused: map[string]bool{}}
}

// Skips the route of the key for the time given
func (s *State) mark(key, reason string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outages[key] = outage{until: time.Now().Add(d), reason: reason}
}

// Why the route of the key is skipped and for how much longer, false once it is back
func (s *State) out(key string) (string, time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.outages[key]
	if !ok {
		return "", 0, false
	}
	left := time.Until(o.until)
	if left <= 0 {
		delete(s.outages, key)
		return "", 0, false
	}
	return o.reason, left, true
}

// Remembers that a route turned down a part of the request, such as the temperature, so later calls leave it out
func (s *State) refuse(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused[key] = true
}

func (s *State) refuses(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused[key]
}

// Whether calls of the profile learned that the route cannot switch reasoning off for its model, so they send the weakest effort instead
func (s *State) Floored(profile string, route provider.Route) bool {
	return s.refuses(floorKey(profile, route))
}

// Remembers for the calls of the profile that the route cannot switch reasoning off for its model
func (s *State) Floor(profile string, route provider.Route) {
	s.refuse(floorKey(profile, route))
}

// Key of what calls learned of the route's model and reasoning switched off, the one a refused call keeps
func floorKey(profile string, route provider.Route) string {
	return profile + "/" + candidate{route: route}.key() + "/" + strings.ToLower(route.Model[strings.LastIndex(route.Model, "/")+1:]) + "/floor"
}
