package acp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/logging"
)

// Timeout of a future wait, the concurrent.futures TimeoutError
var ErrFutureTimeout = errors.New("TimeoutError")

// Cancellation of a scheduled coroutine
var ErrFutureCancelled = errors.New("CancelledError")

// Event loop of the ACP connection that worker threads schedule coroutines on
type Loop interface {
	RunCoroutineThreadsafe(fn func(ctx context.Context) (any, error)) (Future, error)
}

// Pending result of a coroutine scheduled from another thread
type Future interface {
	Result(timeout time.Duration) (any, error)
	Cancel() bool
}

// Loop bound to the lifetime of a connection context
type ConnLoop struct {
	ctx context.Context
}

// Loop running coroutines until ctx is done
func NewConnLoop(ctx context.Context) *ConnLoop { return &ConnLoop{ctx: ctx} }

// Context the loop's coroutines run under
func (l *ConnLoop) Context() context.Context { return l.ctx }

// Starts fn on the loop, refused once the loop is closed
func (l *ConnLoop) RunCoroutineThreadsafe(fn func(ctx context.Context) (any, error)) (Future, error) {
	if l == nil || l.ctx == nil {
		return nil, errors.New("loop is None")
	}
	if l.ctx.Err() != nil {
		return nil, errors.New("Event loop is closed")
	}
	ctx, cancel := context.WithCancel(l.ctx)
	f := &connFuture{done: make(chan struct{}), cancel: cancel}
	go func() {
		res, err := fn(ctx)
		f.mu.Lock()
		if !f.settled {
			f.settled = true
			f.result, f.err = res, err
			close(f.done)
		}
		f.mu.Unlock()
		cancel()
	}()
	return f, nil
}

type connFuture struct {
	mu      sync.Mutex
	done    chan struct{}
	cancel  context.CancelFunc
	settled bool
	result  any
	err     error
}

// Waits up to timeout for the outcome, zero waits forever
func (f *connFuture) Result(timeout time.Duration) (any, error) {
	if timeout <= 0 {
		<-f.done
	} else {
		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case <-f.done:
		case <-t.C:
			return nil, ErrFutureTimeout
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result, f.err
}

// Cancels the coroutine, false once it already finished
func (f *connFuture) Cancel() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settled {
		return false
	}
	f.settled = true
	f.err = ErrFutureCancelled
	close(f.done)
	f.cancel()
	return true
}

// Schedules fn on loop, logging at debug and returning nil when the loop is missing or refuses it
func safeScheduleThreadsafe(fn func(ctx context.Context) (any, error), loop Loop, log *logging.Logger, logMessage string) Future {
	var err error
	if loop == nil {
		err = errors.New("loop is None")
	} else {
		var f Future
		if f, err = loop.RunCoroutineThreadsafe(fn); err == nil {
			return f
		}
	}
	if log == nil {
		log = logging.GetLogger("agent.async_utils")
	}
	if logMessage == "" {
		logMessage = "Failed to schedule coroutine on loop"
	}
	log.Debug(fmt.Sprintf("%s: %s", logMessage, err))
	return nil
}
