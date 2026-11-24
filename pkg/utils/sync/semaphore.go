package syncutil

import (
	"context"
	"errors"
)

// Error of a Release that finds every slot of the semaphore already free
var ErrOverRelease = errors.New("semaphore released more often than acquired")

// Counting semaphore with a fixed number of slots; the zero value has none
type Semaphore struct {
	taken chan struct{}
}

// Semaphore with n free slots, none when n is not positive
func NewSemaphore(n int) *Semaphore {
	if n <= 0 {
		return &Semaphore{}
	}
	return &Semaphore{taken: make(chan struct{}, n)}
}

// Takes a slot when one is free, without waiting, reporting whether it got one
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.taken <- struct{}{}:
		return true
	default:
		return false
	}
}

// Waits for a free slot and takes it, the context's error when ctx ends first
func (s *Semaphore) Acquire(ctx context.Context) error {
	if s.TryAcquire() {
		return nil
	}
	select {
	case s.taken <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Gives a slot back, ErrOverRelease when no slot is taken
func (s *Semaphore) Release() error {
	select {
	case <-s.taken:
		return nil
	default:
		return ErrOverRelease
	}
}
