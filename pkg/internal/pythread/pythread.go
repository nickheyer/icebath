package pythread

import (
	"errors"
	"sync"
	"time"
)

var processStart = time.Now()

// Seconds on the process monotonic clock like time.monotonic, comparable only with other readings
func Monotonic() float64 { return time.Since(processStart).Seconds() }

// Event is threading.Event, a flag goroutines set once and wait on
type Event struct {
	mu   sync.Mutex
	set  bool
	wake chan struct{}
}

func (e *Event) waker() chan struct{} {
	if e.wake == nil {
		e.wake = make(chan struct{})
	}
	return e.wake
}

// Sets the flag and wakes every waiter
func (e *Event) Set() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.set {
		e.set = true
		close(e.waker())
	}
}

// Clears the flag so later waits block again
func (e *Event) Clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set {
		e.set = false
		e.wake = make(chan struct{})
	}
}

// Reports whether the flag is set
func (e *Event) IsSet() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set
}

// Blocks until the flag is set or the timeout in seconds passes, a negative timeout waits forever
func (e *Event) Wait(timeout float64) bool {
	e.mu.Lock()
	if e.set {
		e.mu.Unlock()
		return true
	}
	wake := e.waker()
	e.mu.Unlock()
	if timeout < 0 {
		<-wake
		return true
	}
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-wake:
		return true
	case <-timer.C:
		return e.IsSet()
	}
}

// ErrSemaphoreReleasedTooManyTimes is the ValueError of an over released BoundedSemaphore
var ErrSemaphoreReleasedTooManyTimes = errors.New("Semaphore released too many times")

// BoundedSemaphore is threading.BoundedSemaphore, a counter that refuses release past its initial value
type BoundedSemaphore struct {
	slots chan struct{}
}

// Semaphore holding value free slots
func NewBoundedSemaphore(value int) *BoundedSemaphore {
	s := &BoundedSemaphore{slots: make(chan struct{}, value)}
	for i := 0; i < value; i++ {
		s.slots <- struct{}{}
	}
	return s
}

// Takes a slot, without blocking false when none is free
func (s *BoundedSemaphore) Acquire(blocking bool) bool {
	if blocking {
		<-s.slots
		return true
	}
	select {
	case <-s.slots:
		return true
	default:
		return false
	}
}

// Returns a slot, the ValueError when every slot is already free
func (s *BoundedSemaphore) Release() error {
	select {
	case s.slots <- struct{}{}:
		return nil
	default:
		return ErrSemaphoreReleasedTooManyTimes
	}
}
