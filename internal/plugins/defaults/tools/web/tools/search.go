package tools

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/web/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/web"
)

// Result counts a search asks a backend for, the caller's limit rounded up to one of them so nearby limits share a cached answer
var buckets = []int{10, 20, 50, 100}

func bucket(limit int) int {
	for _, size := range buckets {
		if limit <= size {
			return size
		}
	}
	return buckets[len(buckets)-1]
}

// Answer of the backend for the search, from the cache while it is fresh, one call at a time per search, cut to the limit
func (r *registrant) answer(ctx context.Context, call *tools.Call, backend web.Backend, query string, limit int) (*web.Results, error) {
	cfg := call.Env.Config
	key := memoKey{profile: cfg.Profile(), backend: backend.Name, query: normalQuery(query), limit: bucket(limit)}
	results := r.memo.get(cfg, key)
	if results == nil {
		lock := r.memo.flight(key)
		lock.Lock()
		defer lock.Unlock()
		if results = r.memo.get(cfg, key); results == nil {
			fetched, rescued, err := r.call(ctx, cfg, backend, query, key.limit)
			if err != nil {
				return nil, err
			}
			if !rescued {
				r.memo.put(cfg, key, fetched)
			}
			results = fetched
		}
	}
	if results.Data != nil && len(results.Data.Web) > limit {
		results = results.Clone()
		results.Data.Web = results.Data.Web[:limit]
	}
	return results, nil
}

// Search of the backend, once more on the free ring when it fails and rescue applies; rescued says the ring answered
func (r *registrant) call(ctx context.Context, cfg *config.Config, backend web.Backend, query string, limit int) (*web.Results, bool, error) {
	results, err := r.searchOnce(ctx, cfg, backend, query, limit)
	if err == nil && results == nil {
		results = web.Failed("")
	}
	if err == nil && results.Success || !r.backends.Rescues(cfg, backend) {
		return results, false, err
	}
	failure := ""
	if err != nil {
		failure = err.Error()
	} else {
		failure = results.Error
	}
	rescued, err := r.backends.RescueSearch(ctx, cfg, backend.Name, failure, query, limit)
	return rescued, err == nil, err
}

// One search of the backend: on its free plan through the ring when its calls go there, else through its own API
func (r *registrant) searchOnce(ctx context.Context, cfg *config.Config, backend web.Backend, query string, limit int) (*web.Results, error) {
	if ctx.Err() != nil {
		return web.Failed("Interrupted"), nil
	}
	if backend.Free != nil && backend.Free.Used != nil && backend.Free.Used(cfg) {
		results, err := r.backends.FreeSearch(ctx, cfg, backend.Name, query, limit)
		if err != nil && !backend.Free.Raises {
			return web.Failed(web.Failure(backend.Title, "search", err)), nil
		}
		return results, err
	}
	return backend.Search(ctx, cfg, query, limit)
}

var spaces = regexp.MustCompile(`\s+`)

// Query folded to lower case with its whitespace collapsed, so trivial variants share a cached answer
func normalQuery(query string) string {
	return spaces.ReplaceAllString(strings.ToLower(strings.TrimSpace(query)), " ")
}

type memoKey struct {
	profile, backend, query string
	limit                   int
}

type memoEntry struct {
	expires time.Time
	results *web.Results
}

// Answers of recent searches kept for the cache time, and a lock per search so the same one runs once at a time
type memo struct {
	mu      sync.Mutex
	entries map[memoKey]memoEntry
	locks   map[memoKey]*sync.Mutex
}

func newMemo() *memo {
	return &memo{entries: map[memoKey]memoEntry{}, locks: map[memoKey]*sync.Mutex{}}
}

// Copy of a fresh answer, nil when there is none or the cache is off
func (m *memo) get(cfg *config.Config, key memoKey) *web.Results {
	if !cfg.GetBool(webconfig.CacheEnabled) {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if !ok || !time.Now().Before(entry.expires) {
		delete(m.entries, key)
		return nil
	}
	return entry.results.Clone()
}

// Keeps a successful answer, dropping the expired ones
func (m *memo) put(cfg *config.Config, key memoKey, results *web.Results) {
	if !cfg.GetBool(webconfig.CacheEnabled) || results == nil || !results.Success {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for k, entry := range m.entries {
		if !now.Before(entry.expires) {
			delete(m.entries, k)
		}
	}
	m.entries[key] = memoEntry{expires: now.Add(ttl(cfg)), results: results.Clone()}
}

// Lock held around a miss, the call and the store, so the same search runs once at a time. Past 256 locks the idle ones are dropped
func (m *memo) flight(key memoKey) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lock, ok := m.locks[key]; ok {
		return lock
	}
	if len(m.locks) > 256 {
		for k, lock := range m.locks {
			if lock.TryLock() {
				lock.Unlock()
				delete(m.locks, k)
			}
		}
	}
	lock := &sync.Mutex{}
	m.locks[key] = lock
	return lock
}

// How long a cached result is reused, within the bounds of its setting
func ttl(cfg *config.Config) time.Duration {
	minutes := min(max(cfg.GetInt(webconfig.CacheTTLMinutes), webconfig.MinCacheMinutes), webconfig.MaxCacheMinutes)
	return time.Duration(minutes) * time.Minute
}
