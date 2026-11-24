package syncutil

import "sync"

// Slot count below which a GoroutineLocal never looks for finished goroutines
const localPruneFloor = 256

// Value slot every goroutine sees on its own, keyed by GoroutineID; slots of finished goroutines are dropped as the slot count grows, and the zero value is empty and ready to use
type GoroutineLocal[T any] struct {
	mu      sync.RWMutex
	vals    map[uint64]T
	pruneAt int
}

// Value the calling goroutine stored, false when it stored none
func (l *GoroutineLocal[T]) Get() (T, bool) {
	id := GoroutineID()
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.vals[id]
	return v, ok
}

// Stores v for the calling goroutine only
func (l *GoroutineLocal[T]) Set(v T) {
	id := GoroutineID()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.vals == nil {
		l.vals = make(map[uint64]T)
	}
	l.vals[id] = v
	if len(l.vals) >= max(l.pruneAt, localPruneFloor) {
		kept := make(map[uint64]T, len(l.vals))
		eachLiveGoroutine(func(live uint64) {
			if held, ok := l.vals[live]; ok {
				kept[live] = held
			}
		})
		l.vals = kept
		l.pruneAt = 2 * len(kept)
	}
}

// Forgets the calling goroutine's value
func (l *GoroutineLocal[T]) Delete() {
	id := GoroutineID()
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.vals, id)
}
