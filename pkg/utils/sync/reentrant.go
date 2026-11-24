package syncutil

import (
	"sync/atomic"
	"time"
)

// Mutex the goroutine holding it may lock again, freed for others once every Lock has had its Unlock; the zero value is unlocked and ready to use, and it must not be copied after first use
type ReentrantMutex struct {
	slot  atomic.Pointer[chan struct{}]
	owner atomic.Uint64
	depth int
}

// Channel of capacity one whose single element is the lock, made on first use
func (m *ReentrantMutex) sem() chan struct{} {
	if p := m.slot.Load(); p != nil {
		return *p
	}
	ch := make(chan struct{}, 1)
	if m.slot.CompareAndSwap(nil, &ch) {
		return ch
	}
	return *m.slot.Load()
}

// Blocks until the mutex is free or already held by the calling goroutine, then takes one more hold
func (m *ReentrantMutex) Lock() {
	id := GoroutineID()
	if m.owner.Load() == id {
		m.depth++
		return
	}
	m.sem() <- struct{}{}
	m.own(id)
}

// Takes one more hold like Lock but gives up once d has passed, reporting whether it got the mutex; a d of zero or less only tries
func (m *ReentrantMutex) TryLockFor(d time.Duration) bool {
	id := GoroutineID()
	if m.owner.Load() == id {
		m.depth++
		return true
	}
	sem := m.sem()
	select {
	case sem <- struct{}{}:
		m.own(id)
		return true
	default:
	}
	if d <= 0 {
		return false
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
		m.own(id)
		return true
	case <-timer.C:
		return false
	}
}

func (m *ReentrantMutex) own(id uint64) {
	m.owner.Store(id)
	m.depth = 1
}

// Drops one hold of the calling goroutine, freeing the mutex when no hold is left; panics when the calling goroutine does not hold it
func (m *ReentrantMutex) Unlock() {
	if m.owner.Load() != GoroutineID() {
		panic("syncutil: unlock of ReentrantMutex not held by this goroutine")
	}
	m.depth--
	if m.depth == 0 {
		m.owner.Store(0)
		<-m.sem()
	}
}
