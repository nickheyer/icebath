package syncutil

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Flag goroutines raise and wait on, lowered again by Clear; the zero Event is lowered and ready to use, and an Event must not be copied after first use
type Event struct {
	set  atomic.Bool
	mu   sync.Mutex
	wake chan struct{}
}

// Channel already closed, handed out while the flag is up and no waiter made a channel of its own
var closedChan = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// Raises the flag and wakes every waiter
func (e *Event) Set() {
	if e.set.Load() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set.Load() {
		return
	}
	e.set.Store(true)
	if e.wake == nil {
		e.wake = closedChan
	} else {
		close(e.wake)
	}
}

// Lowers the flag so later waits block until the next Set
func (e *Event) Clear() {
	if !e.set.Load() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set.Load() {
		e.wake = nil
		e.set.Store(false)
	}
}

// Whether the flag is up
func (e *Event) IsSet() bool { return e.set.Load() }

// Channel closed once the flag is up, for a select arm; after a Clear the next call hands out a fresh channel
func (e *Event) Done() <-chan struct{} {
	if e.set.Load() {
		return closedChan
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.wake == nil {
		if e.set.Load() {
			e.wake = closedChan
		} else {
			e.wake = make(chan struct{})
		}
	}
	return e.wake
}

// Blocks until the flag is up
func (e *Event) Wait() {
	if !e.set.Load() {
		<-e.Done()
	}
}

// Blocks until the flag is up or d has passed, reporting whether it is up; a d of zero or less only looks
func (e *Event) WaitTimeout(d time.Duration) bool {
	if e.set.Load() {
		return true
	}
	if d <= 0 {
		return false
	}
	done := e.Done()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return e.set.Load()
	}
}

// Blocks until the flag is up or ctx ends, nil once it is up and the context's error otherwise
func (e *Event) WaitContext(ctx context.Context) error {
	if e.set.Load() {
		return nil
	}
	select {
	case <-e.Done():
		return nil
	case <-ctx.Done():
		if e.set.Load() {
			return nil
		}
		return ctx.Err()
	}
}
