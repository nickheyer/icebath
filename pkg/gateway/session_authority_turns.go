package gateway

import (
	"context"
	"sync"
)

// First in first out lock a waiter leaves when its context is cancelled, the asyncio Lock of hermes
type asyncLock struct {
	once sync.Once
	slot chan struct{}
}

// Takes the lock, an error when the context ends first
func (l *asyncLock) Lock(ctx context.Context) error {
	l.once.Do(func() { l.slot = make(chan struct{}, 1) })
	select {
	case l.slot <- struct{}{}:
		return nil
	default:
	}
	select {
	case l.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Releases the lock to the next waiter
func (l *asyncLock) Unlock() {
	<-l.slot
}

// Lock of one key with the count of its holder and waiters
type keyedTurnEntry struct {
	lock  asyncLock
	users int
}

// Runs body as one ordered turn per key, queued firing when an earlier turn holds or awaits it
func keyedTurn(ctx context.Context, mu *sync.Mutex, table map[string]*keyedTurnEntry, key string, queued func(), body func(ctx context.Context) error) error {
	mu.Lock()
	entry := table[key]
	if entry == nil {
		entry = &keyedTurnEntry{}
		table[key] = entry
	}
	entry.users++
	behind := entry.users > 1
	mu.Unlock()
	defer func() {
		mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(table, key)
		}
		mu.Unlock()
	}()
	if queued != nil && behind {
		queued()
	}
	if err := entry.lock.Lock(ctx); err != nil {
		return err
	}
	defer entry.lock.Unlock()
	return body(ctx)
}
