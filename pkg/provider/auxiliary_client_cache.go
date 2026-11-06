package provider

import (
	"reflect"
	"slices"
	"sync"
)

// Auxiliary client cache key, the hermes tuple with every composite slot rendered secret free
type auxClientCacheKey struct {
	Home     string
	Provider string
	Async    bool
	BaseURL  string
	APIKey   string
	APIMode  string
	Runtime  string
	Vision   bool
	Task     string
	PoolHint string
	Model    string
	Borrowed string
}

// Cached client with its default model and the event loop it was bound to
type auxCachedClient struct {
	Client       AuxClient
	DefaultModel string
	BoundLoop    any
}

// Oldest entries are evicted once the cache grows past this size
const auxClientCacheMaxSize = 64

var (
	auxClientCacheMu    sync.Mutex
	auxClientCache      = map[auxClientCacheKey]auxCachedClient{}
	auxClientCacheOrder []auxClientCacheKey
)

// Cache keys in insertion order, the caller holds the cache lock
func auxClientCacheKeysLocked() []auxClientCacheKey {
	return slices.Clone(auxClientCacheOrder)
}

// Stores an entry keeping the slot of an existing key, the caller holds the cache lock
func auxClientCacheSetLocked(key auxClientCacheKey, entry auxCachedClient) {
	if _, ok := auxClientCache[key]; !ok {
		auxClientCacheOrder = append(auxClientCacheOrder, key)
	}
	auxClientCache[key] = entry
}

// Removes an entry and its order slot, the caller holds the cache lock
func auxClientCacheDeleteLocked(key auxClientCacheKey) {
	if _, ok := auxClientCache[key]; !ok {
		return
	}
	delete(auxClientCache, key)
	if i := slices.Index(auxClientCacheOrder, key); i >= 0 {
		auxClientCacheOrder = slices.Delete(auxClientCacheOrder, i, i+1)
	}
}

// Identity comparison of two values, pointer identity for reference kinds and equality for comparable values
func auxSameObject(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	switch va.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Slice:
		return va.Pointer() == vb.Pointer()
	}
	return va.Type().Comparable() && a == b
}

// Drops cache entries whose client or its real client is the target, true when any was dropped
func auxEvictCachedClientInstance(target any) bool {
	if target == nil {
		return false
	}
	if v := reflect.ValueOf(target); v.Kind() == reflect.Pointer && v.IsNil() {
		return false
	}
	evicted := false
	auxClientCacheMu.Lock()
	defer auxClientCacheMu.Unlock()
	for _, key := range auxClientCacheKeysLocked() {
		cached := auxClientCache[key].Client
		if cached == nil {
			continue
		}
		var real any
		if holder, ok := cached.(interface{ auxRealClient() AuxClient }); ok {
			if rc := holder.auxRealClient(); rc != nil {
				real = rc
			}
		}
		if auxSameObject(cached, target) || auxSameObject(real, target) {
			auxClientCacheDeleteLocked(key)
			evicted = true
		}
	}
	return evicted
}

// Event loop an async cached client is bound to, stale once it reports closed
type auxBoundLoop interface {
	IsClosed() bool
}

// Closes one cached client, a raised close error swallowed since the transport is dropped either way
func auxCloseCachedClient(client AuxClient) {
	if client == nil {
		return
	}
	_ = auxRecoverRaise(func() { _ = client.Close() })
}

// CleanupStaleAsyncClients drops and closes every cached async client whose bound loop has closed
func CleanupStaleAsyncClients() {
	auxClientCacheMu.Lock()
	var stale []AuxClient
	for _, k := range auxClientCacheKeysLocked() {
		entry := auxClientCache[k]
		if loop, ok := entry.BoundLoop.(auxBoundLoop); ok && loop.IsClosed() {
			stale = append(stale, entry.Client)
			auxClientCacheDeleteLocked(k)
		}
	}
	auxClientCacheMu.Unlock()
	for _, client := range stale {
		auxCloseCachedClient(client)
	}
}
