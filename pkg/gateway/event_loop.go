package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrLoopClosed is the asyncio RuntimeError of scheduling onto a closed event loop
var ErrLoopClosed = errors.New("Event loop is closed")

// Callback scheduling surface of the gateway runner loop that the liveness backstops drive
type EventLoop interface {
	CallSoonThreadsafe(fn func()) error
	CallLater(delay time.Duration, fn func()) (LoopTimer, error)
	IsClosed() bool
	IsRunning() bool
}

// Pending loop timer that can be cancelled before its callback runs
type LoopTimer interface {
	Cancel()
}

// Single goroutine callback loop the gateway runner runs its work on, the asyncio event loop of the runner
type CallbackLoop struct {
	mu      sync.Mutex
	queue   []*loopCallback
	wake    chan struct{}
	closed  bool
	running bool
}

type loopCallback struct {
	mu        sync.Mutex
	fn        func()
	cancelled bool
	timer     *time.Timer
}

// Stops the timer and drops the callback if it has not run yet
func (c *loopCallback) Cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelled = true
	if c.timer != nil {
		c.timer.Stop()
	}
}

func NewCallbackLoop() *CallbackLoop {
	return &CallbackLoop{wake: make(chan struct{}, 1)}
}

func (l *CallbackLoop) enqueue(cb *loopCallback) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrLoopClosed
	}
	l.queue = append(l.queue, cb)
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return nil
}

// Queues fn to run on the loop goroutine from any goroutine, ErrLoopClosed once the loop is closed
func (l *CallbackLoop) CallSoonThreadsafe(fn func()) error {
	return l.enqueue(&loopCallback{fn: fn})
}

// Starts fn from the loop goroutine as its own task beside the loop callbacks, false once the loop is closed
func (l *CallbackLoop) Schedule(fn func()) bool {
	return l.CallSoonThreadsafe(func() { go fn() }) == nil
}

// Runs fn on the loop after delay, ErrLoopClosed once the loop is closed
func (l *CallbackLoop) CallLater(delay time.Duration, fn func()) (LoopTimer, error) {
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return nil, ErrLoopClosed
	}
	cb := &loopCallback{fn: fn}
	cb.mu.Lock()
	cb.timer = time.AfterFunc(max(delay, 0), func() { l.enqueue(cb) })
	cb.mu.Unlock()
	return cb, nil
}

// Closes the loop so later scheduling fails and Run returns, pending callbacks dropped
func (l *CallbackLoop) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.queue = nil
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Runs queued callbacks one at a time on the calling goroutine until ctx ends or the loop closes
func (l *CallbackLoop) Run(ctx context.Context) error {
	l.mu.Lock()
	l.running = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.running = false
		l.mu.Unlock()
	}()
	for {
		l.mu.Lock()
		closed, batch := l.closed, l.queue
		l.queue = nil
		l.mu.Unlock()
		if closed {
			return nil
		}
		for _, cb := range batch {
			cb.mu.Lock()
			skip := cb.cancelled
			cb.mu.Unlock()
			if !skip {
				l.runCallback(cb.fn)
			}
		}
		if len(batch) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.wake:
		}
	}
}

// Runs one callback, logging a panic the way asyncio logs an exception in a callback
func (l *CallbackLoop) runCallback(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			exc, ok := r.(error)
			if !ok {
				exc = fmt.Errorf("%v", r)
			}
			l.CallExceptionHandler(LoopExceptionContext{Message: fmt.Sprintf("Exception in callback %T", fn), Exception: exc})
		}
	}()
	fn()
}
