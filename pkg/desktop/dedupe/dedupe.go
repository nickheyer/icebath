package dedupe

import (
	"strings"
	"sync"
	"time"
)

// Window in which a repeated key from another app window is dropped
const DefaultInterval = time.Second

// How long a spoken reply stays claimed so a late window does not read it twice
const SpeechClaimTTL = 10 * time.Minute

// Answers whether key was already claimed within the interval, pruning stale keys on every call
type Deduper func(key string, now time.Time) bool

// Deduper over the given interval, safe for concurrent windows
func NewEventDeduper(interval time.Duration) Deduper {
	var mu sync.Mutex
	lastSeenAt := map[string]time.Time{}
	return func(key string, now time.Time) bool {
		mu.Lock()
		defer mu.Unlock()
		for k, at := range lastSeenAt {
			if now.Sub(at) >= interval {
				delete(lastSeenAt, k)
			}
		}
		if _, ok := lastSeenAt[key]; ok {
			return true
		}
		lastSeenAt[key] = now
		return false
	}
}

// True when this window owns an ambient cue, speak keys holding for the speech TTL and others for the tick window
type Arbiter func(key string, now time.Time) bool

// Cross window arbiter of every ambient cue
func NewAmbientClaimArbiter(interval, speechTTL time.Duration) Arbiter {
	cues := NewEventDeduper(interval)
	speech := NewEventDeduper(speechTTL)
	return func(key string, now time.Time) bool {
		if strings.HasPrefix(key, "speak:") {
			return !speech(key, now)
		}
		return !cues(key, now)
	}
}
