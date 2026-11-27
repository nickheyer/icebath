package secrets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// SHA-256 prefix of credential material a cache is keyed by, never shown
func Fingerprint(material string) string {
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])[:16]
}

// Secrets fetched once, with when
type Entry struct {
	Secrets map[string]string
	Fetched time.Time
}

// Whether the entry is younger than ttl, never for no ttl
func (e Entry) Fresh(ttl time.Duration) bool {
	return ttl > 0 && time.Since(e.Fetched) < ttl
}

// Entry of a cache file's {"secrets", "fetched_at"} object, false when it is not one; only text values are kept
func Decode(payload map[string]any) (Entry, bool) {
	raw, ok := payload["secrets"].(map[string]any)
	if !ok {
		return Entry{}, false
	}
	at, ok := payload["fetched_at"].(float64)
	if !ok || math.IsNaN(at) || math.IsInf(at, 0) {
		return Entry{}, false
	}
	secrets := map[string]string{}
	for name, value := range raw {
		if text, ok := value.(string); ok {
			secrets[name] = text
		}
	}
	whole, fraction := math.Modf(at)
	return Entry{Secrets: secrets, Fetched: time.Unix(int64(whole), int64(fraction*1e9))}, true
}

// {"secrets", "fetched_at"} object of an entry, fetched_at in seconds since the epoch
func Encode(entry Entry) map[string]any {
	return map[string]any{"secrets": entry.Secrets, "fetched_at": float64(entry.Fetched.UnixNano()) / 1e9}
}

// Writes a cache file only its owner reads, at once, its folder tightened to 0700
func WriteJSON(path string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	return fsx.Write(path, data, 0o600)
}

// What a store fetched kept in this process and in a file of the profile folder, so a start within the ttl asks the store nothing
type Cache struct {
	// File name in the profile folder
	Name   string
	mu     sync.Mutex
	memory map[string]Entry
}

// File of the cache in the profile folder
func (c *Cache) Path(dir string) string {
	return filepath.Join(dir, c.Name)
}

// Fresh entry of the key in the file, false when it is another key's, stale, or no entry; an error naming the file only for bytes that are
// not text
func (c *Cache) Read(dir, key string, ttl time.Duration) (Entry, bool, error) {
	if ttl <= 0 {
		return Entry{}, false, nil
	}
	path := c.Path(dir)
	data, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, false, nil
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return Entry{}, false, fmt.Errorf("secret cache %s is not UTF-8 text; delete it to fetch again", path)
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return Entry{}, false, nil
	}
	if stored, ok := payload["key"].(string); !ok || stored != key {
		return Entry{}, false, nil
	}
	entry, ok := Decode(payload)
	if !ok || !entry.Fresh(ttl) {
		return Entry{}, false, nil
	}
	return entry, true, nil
}

// Writes the entry to the file, nothing when there is no ttl or the write fails
func (c *Cache) Write(dir, key string, entry Entry, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	payload := Encode(entry)
	payload["key"] = key
	_ = WriteJSON(c.Path(dir), payload)
}

// Fresh entry of the key from this process, else from the file, or from read when one is given, which this process keeps from then on
func (c *Cache) Lookup(dir, key string, ttl time.Duration, read func() (Entry, bool)) (Entry, bool, error) {
	c.mu.Lock()
	held, ok := c.memory[key]
	c.mu.Unlock()
	if ok && held.Fresh(ttl) {
		return held, true, nil
	}
	var entry Entry
	var found bool
	if read != nil {
		entry, found = read()
	} else {
		var err error
		if entry, found, err = c.Read(dir, key, ttl); err != nil {
			return Entry{}, false, err
		}
	}
	if found {
		c.Remember(key, entry)
	}
	return entry, found, nil
}

// Keeps the entry in this process
func (c *Cache) Remember(key string, entry Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.memory == nil {
		c.memory = map[string]Entry{}
	}
	c.memory[key] = Entry{Secrets: maps.Clone(entry.Secrets), Fetched: entry.Fetched}
}

// Keeps the entry in this process and in the file
func (c *Cache) Store(dir, key string, entry Entry, ttl time.Duration) {
	c.Remember(key, entry)
	c.Write(dir, key, entry, ttl)
}

// Forgets every entry, the file's too
func (c *Cache) Clear(dir string) {
	c.mu.Lock()
	clear(c.memory)
	c.mu.Unlock()
	RemoveFile(c.Path(dir))
}

// Deletes a file, or a link where one stands, leaving a folder at the path alone
func RemoveFile(path string) {
	if info, err := os.Lstat(path); err == nil && !info.IsDir() {
		_ = os.Remove(path)
	}
}
