package connections

import (
	"sync"
	"time"
)

// RemoteOwnerEntry is a remembered remote owner of a session id, an empty profile when no remote owns it
type RemoteOwnerEntry struct {
	At      int64
	Profile string
}

// RemoteOwnerCache is a bounded, time limited memo of which remote profile owns a session id
type RemoteOwnerCache struct {
	mu      sync.Mutex
	limit   int
	ttlMs   int64
	order   []string
	entries map[string]RemoteOwnerEntry
}

// NewRemoteOwnerCache holds up to 500 owners for 30 seconds each
func NewRemoteOwnerCache() *RemoteOwnerCache {
	return &RemoteOwnerCache{limit: 500, ttlMs: 30000, entries: map[string]RemoteOwnerEntry{}}
}

// Fresh answers the remembered owner while it is younger than the TTL
func (c *RemoteOwnerCache) Fresh(sessionID string) (RemoteOwnerEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.entries[sessionID]
	if ok && time.Now().UnixMilli()-cached.At < c.ttlMs {
		return cached, true
	}
	return RemoteOwnerEntry{}, false
}

// Remember stores an owner, evicting the oldest inserted id once the cache is full
func (c *RemoteOwnerCache) Remember(sessionID, profile string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.limit && len(c.order) > 0 {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	if _, ok := c.entries[sessionID]; !ok {
		c.order = append(c.order, sessionID)
	}
	c.entries[sessionID] = RemoteOwnerEntry{At: time.Now().UnixMilli(), Profile: profile}
}
