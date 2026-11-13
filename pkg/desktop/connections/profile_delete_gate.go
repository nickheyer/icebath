package connections

import (
	"fmt"
	"strings"
	"sync"
)

// ProfileDeletionGate counts in flight deletions per profile so no backend respawns a profile being removed
type ProfileDeletionGate struct {
	mu     sync.Mutex
	active map[string]int
}

func deletionGateKey(profile string) string { return strings.ToLower(jsTrim(profile)) }

// Acquire holds the profile until the returned release runs, once, an empty profile holding nothing
func (g *ProfileDeletionGate) Acquire(profile string) func() {
	key := deletionGateKey(profile)
	if key == "" {
		return func() {}
	}
	g.mu.Lock()
	if g.active == nil {
		g.active = map[string]int{}
	}
	g.active[key]++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			remaining, ok := g.active[key]
			if !ok {
				remaining = 1
			}
			remaining--
			if remaining > 0 {
				g.active[key] = remaining
			} else {
				delete(g.active, key)
			}
		})
	}
}

// Blocks reports a profile some deletion still holds
func (g *ProfileDeletionGate) Blocks(profile string) bool {
	key := deletionGateKey(profile)
	g.mu.Lock()
	defer g.mu.Unlock()
	return key != "" && g.active[key] > 0
}

// AssertCanStart refuses a profile whose deletion is in flight
func (g *ProfileDeletionGate) AssertCanStart(profile string) error {
	key := jsTrim(profile)
	if g.Blocks(key) {
		return fmt.Errorf("Profile \"%s\" is being deleted.", key)
	}
	return nil
}
