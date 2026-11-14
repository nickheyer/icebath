package secret

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// First sixteen hex digits of the SHA-256 of material, the form auth material takes inside a cache key
func fingerprint(material string) string {
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])[:16]
}

// Fetched secret values with the instant they were fetched, in seconds since the epoch
type cachedFetch struct {
	secrets   map[string]string
	fetchedAt float64
}

// Reports an entry younger than ttl, never one under a ttl of zero
func (c cachedFetch) fresh(ttl float64) bool {
	return ttl > 0 && nowSeconds()-c.fetchedAt < ttl
}

// Wall clock in fractional seconds since the epoch
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Cache entry from its decoded JSON object, false when malformed, only string values kept
func entryFromPayload(payload map[string]json.RawMessage) (cachedFetch, bool) {
	var raw map[string]json.RawMessage
	var fetchedAt float64
	if json.Unmarshal(payload["secrets"], &raw) != nil || raw == nil || json.Unmarshal(payload["fetched_at"], &fetchedAt) != nil {
		return cachedFetch{}, false
	}
	secrets := map[string]string{}
	for name, value := range raw {
		var text string
		if json.Unmarshal(value, &text) == nil && string(value) != "null" {
			secrets[name] = text
		}
	}
	return cachedFetch{secrets: secrets, fetchedAt: fetchedAt}, true
}

// Cache file at the top of a home, a copy earlier releases kept under the cache folder deleted unread
func currentCachePath(homeDir string, entry home.Entry) string {
	_ = os.Remove(filepath.Join(home.Cache.In(homeDir), string(entry)))
	return entry.In(homeDir)
}

// Deletes a cache file and its older copy under the cache folder
func removeCacheFile(homeDir string, entry home.Entry) {
	_ = os.Remove(entry.In(homeDir))
	_ = os.Remove(filepath.Join(home.Cache.In(homeDir), string(entry)))
}

// Writes a cache object owner only on one line, its folder tightened to the owner
func writeCacheJSON(ctx context.Context, path string, payload any) error {
	if err := home.AssertProfileLive(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = home.SecureParentDir(path)
	return fileutil.WriteJSON(ctx, path, payload, fileutil.JSONStyle{ASCII: true}, fileutil.Private())
}

// Cache of fetched values, in process over an owner only file at the top of each home
type secretCache struct {
	entry home.Entry

	mu     sync.Mutex
	memory map[string]cachedFetch
}

// Empty cache persisting under entry
func newSecretCache(entry home.Entry) *secretCache {
	return &secretCache{entry: entry, memory: map[string]cachedFetch{}}
}

// Fresh disk entry stored under key, false when absent, unreadable, for another key or stale
func (c *secretCache) readDisk(homeDir, key string, ttl float64) (cachedFetch, bool) {
	if ttl <= 0 {
		return cachedFetch{}, false
	}
	var payload map[string]json.RawMessage
	if fileutil.ReadJSON(currentCachePath(homeDir, c.entry), &payload) != nil {
		return cachedFetch{}, false
	}
	var stored string
	if json.Unmarshal(payload["key"], &stored) != nil || stored != key {
		return cachedFetch{}, false
	}
	entry, ok := entryFromPayload(payload)
	if !ok || !entry.fresh(ttl) {
		return cachedFetch{}, false
	}
	return entry, true
}

// Persists an entry under key, nothing under a ttl of zero, a failed write leaving a later miss
func (c *secretCache) writeDisk(ctx context.Context, homeDir, key string, entry cachedFetch, ttl float64) {
	if ttl <= 0 {
		return
	}
	payload := map[string]any{"key": key, "secrets": entry.secrets, "fetched_at": entry.fetchedAt}
	_ = writeCacheJSON(ctx, currentCachePath(homeDir, c.entry), payload)
}

// Fresh entry from memory, else from disk and promoted into memory, readDisk replacing the plain file reader when set
func (c *secretCache) lookup(homeDir, memoryKey, diskKey string, ttl float64, readDisk func() (cachedFetch, bool)) (cachedFetch, bool) {
	c.mu.Lock()
	cached, ok := c.memory[memoryKey]
	c.mu.Unlock()
	if ok && cached.fresh(ttl) {
		return cached, true
	}
	var entry cachedFetch
	if readDisk != nil {
		entry, ok = readDisk()
	} else {
		entry, ok = c.readDisk(homeDir, diskKey, ttl)
	}
	if ok {
		c.remember(memoryKey, entry)
	}
	return entry, ok
}

// Keeps an entry in memory under key
func (c *secretCache) remember(memoryKey string, entry cachedFetch) {
	c.mu.Lock()
	c.memory[memoryKey] = entry
	c.mu.Unlock()
}

// Keeps an entry in memory and on disk
func (c *secretCache) store(ctx context.Context, homeDir, memoryKey, diskKey string, entry cachedFetch, ttl float64) {
	c.remember(memoryKey, entry)
	c.writeDisk(ctx, homeDir, diskKey, entry, ttl)
}

// Drops every memory entry and the cache file of a home
func (c *secretCache) clear(homeDir string) {
	c.mu.Lock()
	c.memory = map[string]cachedFetch{}
	c.mu.Unlock()
	removeCacheFile(homeDir, c.entry)
}
