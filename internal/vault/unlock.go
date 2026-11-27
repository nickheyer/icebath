package vault

import (
	"sync"
	"time"
)

// Longest a password manager stays unlocked without a call
const idle = 30 * time.Minute

// Profile and password manager a session token is kept for
type tokenKey struct{ profile, backend string }

type token struct {
	value string
	used  time.Time
	// Session of icebath it was unlocked for, empty for one unlocked from the app or the command line
	owner string
}

// Session tokens of the unlocked password managers: each forgotten once idle, its session ends or it is locked; a lock bumps the
// generation of its key so an unlock begun before it can't land after it
type tokens struct {
	mu          sync.Mutex
	held        map[tokenKey]*token
	generations map[tokenKey]int
	timer       *time.Timer
}

func newTokens() *tokens {
	return &tokens{held: map[tokenKey]*token{}, generations: map[tokenKey]int{}}
}

// Token of the key while it is live, touching it when the call is a real use of the manager
func (t *tokens) live(key tokenKey, touch bool) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	held := t.held[key]
	if held == nil {
		return "", false
	}
	if time.Since(held.used) > idle {
		delete(t.held, key)
		return "", false
	}
	if touch {
		held.used = time.Now()
	}
	return held.value, true
}

// Generation of the key as an unlock begins
func (t *tokens) begin(key tokenKey) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.generations[key]
}

// Keeps the token an unlock got, false when the key was locked since it began
func (t *tokens) commit(key tokenKey, value string, generation int, owner string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if generation != t.generations[key] {
		return false
	}
	t.held[key] = &token{value: value, used: time.Now(), owner: owner}
	t.arm()
	return true
}

// Forgets the token of the key and bumps its generation
func (t *tokens) forget(key tokenKey) {
	delete(t.held, key)
	t.generations[key]++
}

// Locks one manager of the profile, every one for no name
func (t *tokens) lock(profile, backend string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := map[tokenKey]bool{}
	for key := range t.held {
		keys[key] = true
	}
	for key := range t.generations {
		keys[key] = true
	}
	if backend != "" {
		keys[tokenKey{profile, backend}] = true
	}
	for key := range keys {
		if key.profile == profile && (backend == "" || key.backend == backend) {
			t.forget(key)
		}
	}
}

// Locks what the session unlocked
func (t *tokens) release(session string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, held := range t.held {
		if held.owner == session {
			t.forget(key)
		}
	}
}

// Locks every manager of every profile
func (t *tokens) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.held {
		t.forget(key)
	}
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// Wakes when the oldest token goes idle, to forget it and those gone idle with it
func (t *tokens) arm() {
	if t.timer != nil || len(t.held) == 0 {
		return
	}
	var earliest time.Time
	for _, held := range t.held {
		if earliest.IsZero() || held.used.Before(earliest) {
			earliest = held.used
		}
	}
	t.timer = time.AfterFunc(max(time.Until(earliest.Add(idle)), 0)+10*time.Millisecond, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.timer = nil
		for key, held := range t.held {
			if time.Since(held.used) > idle {
				delete(t.held, key)
			}
		}
		t.arm()
	})
}
