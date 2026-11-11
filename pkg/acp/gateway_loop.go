package acp

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Cooperative loop of the gateway agent, one coroutine running at a time between its awaits
type coLoop struct {
	mu      sync.Mutex
	changed chan struct{}
}

func newCoLoop() *coLoop { return &coLoop{changed: make(chan struct{})} }

// Starts running a coroutine on the loop
func (l *coLoop) enter() { l.mu.Lock() }

// Ends the coroutine's run on the loop
func (l *coLoop) leave() { l.mu.Unlock() }

// Suspends the running coroutine around a blocking call so other coroutines run
func (l *coLoop) await(fn func()) {
	l.mu.Unlock()
	defer l.mu.Lock()
	fn()
}

// Suspends the coroutine around a blocking call, cancellation answered at once and its panic raised in the coroutine
func (l *coLoop) awaitCtx(ctx context.Context, fn func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		fn()
	}()
	var err error
	var raised any
	l.await(func() {
		select {
		case raised = <-done:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	if raised != nil {
		panic(raised)
	}
	return err
}

// Wakes every coroutine waiting on the loop's condition
func (l *coLoop) notifyAll() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// Suspends the coroutine until pred holds, the context error when it is cancelled first
func (l *coLoop) waitFor(ctx context.Context, pred func() bool) error {
	for !pred() {
		changed := l.changed
		var err error
		l.await(func() {
			select {
			case <-changed:
			case <-ctx.Done():
				err = ctx.Err()
			}
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Lock coroutines take in turn across their awaits
type coLock struct{ held chan struct{} }

func newCoLock() *coLock { return &coLock{held: make(chan struct{}, 1)} }

// Takes the lock, suspending the coroutine while another holds it
func (k *coLock) acquire(ctx context.Context, l *coLoop) error {
	var err error
	l.await(func() {
		select {
		case k.held <- struct{}{}:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	return err
}

func (k *coLock) release() { <-k.held }

// Coroutine scheduled on the loop with its outcome
type loopTask struct {
	cancel context.CancelFunc
	done   chan struct{}
	result any
	err    error
}

// Asks the task to stop at its next await
func (t *loopTask) Cancel() { t.cancel() }

// Whether the task ended by cancellation
func (t *loopTask) cancelled() bool { return errors.Is(t.err, context.Canceled) }

// Whether the task has finished
func (t *loopTask) finished() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Schedules fn as a coroutine of the loop, onDone running on the loop once it finished
func (l *coLoop) createTask(parent context.Context, fn func(ctx context.Context) (any, error), onDone func(t *loopTask)) *loopTask {
	ctx, cancel := context.WithCancel(parent)
	t := &loopTask{cancel: cancel, done: make(chan struct{})}
	go func() {
		l.mu.Lock()
		func() {
			defer func() {
				if r := recover(); r != nil {
					if failure, ok := r.(error); ok {
						t.err = failure
					} else {
						t.err = fmt.Errorf("%v", r)
					}
				}
			}()
			t.result, t.err = fn(ctx)
		}()
		if onDone != nil {
			onDone(t)
		}
		close(t.done)
		l.mu.Unlock()
		cancel()
	}()
	return t
}

// Suspends the coroutine until the task finished
func (l *coLoop) join(t *loopTask) {
	l.await(func() { <-t.done })
}
