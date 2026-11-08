package provider

import (
	"slices"
	"sync"
	"time"
)

// Pops every key whose stamp, a missing stamp counting as zero, is ttl old
func DropExpired[K comparable, V any](values map[K]V, stamps map[K]time.Time, ttl time.Duration, now time.Time) {
	for k := range values {
		if now.Sub(stamps[k]) >= ttl {
			delete(values, k)
			delete(stamps, k)
		}
	}
}

// Drops the oldest stamped key from cache and every sibling once a new key would pass the limit
func EvictOldest[K comparable, V any](cache map[K]V, key K, stampOf func(V) time.Time, limit int, siblings ...func(K)) {
	if _, has := cache[key]; has || len(cache) < limit {
		return
	}
	var oldest K
	var oldestAt time.Time
	first := true
	for k, v := range cache {
		if at := stampOf(v); first || at.Before(oldestAt) {
			oldest, oldestAt, first = k, at, false
		}
	}
	delete(cache, oldest)
	for _, drop := range siblings {
		drop(oldest)
	}
}

// Stores key as most recently used and drops least recently used keys past limit from table and siblings, returning them
func PublishLRU[K comparable, V any](table map[K]V, order *[]K, key K, value V, limit int, siblings ...func(K)) []K {
	delete(table, key)
	if i := slices.Index(*order, key); i >= 0 {
		*order = slices.Delete(*order, i, i+1)
	}
	table[key] = value
	*order = append(*order, key)
	var evicted []K
	for len(table) > limit {
		stale := (*order)[0]
		*order = (*order)[1:]
		evicted = append(evicted, stale)
		delete(table, stale)
		for _, drop := range siblings {
			drop(stale)
		}
	}
	return evicted
}

// Table of the loop under the lock, tables of loops closed since the last call dropped, keys strong on purpose
func LiveLoopTable[T any](tables map[any]T, loop any, lock sync.Locker, same func(a, b any) bool, closed func(any) bool, newTable func() T) T {
	lock.Lock()
	defer lock.Unlock()
	for other := range tables {
		if !same(other, loop) && closed(other) {
			delete(tables, other)
		}
	}
	table, ok := tables[loop]
	if !ok {
		table = newTable()
		tables[loop] = table
	}
	return table
}
