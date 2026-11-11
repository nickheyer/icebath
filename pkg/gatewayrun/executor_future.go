package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// States of a pool future, the concurrent.futures.Future of hermes
const (
	futurePending = iota
	futureRunning
	futureCancelled
	futureCancelledAndNotified
	futureFinished
)

// Error a cancelled pool future raises, concurrent.futures.CancelledError
var errFutureCancelled = errors.New("CancelledError")

// Error a pool future wait raises when its timeout passes first, TimeoutError
var errFutureTimeout = errors.New("TimeoutError")

// Python InvalidStateError of a future settled twice
type futureInvalidStateError struct{ msg string }

func (e *futureInvalidStateError) Error() string { return e.msg }

// Exception class the error reports, InvalidStateError
func (e *futureInvalidStateError) PyTypeName() string { return "InvalidStateError" }

// Result slot of one pool work item, settled by the worker that ran it
type concurrentFuture struct {
	mu     sync.Mutex
	state  int
	result any
	err    error
	done   chan struct{}
}

// Pending pool future
func newConcurrentFuture() *concurrentFuture {
	return &concurrentFuture{done: make(chan struct{})}
}

// Cancels a future that has not started, false once it runs or finished
func (f *concurrentFuture) cancel() bool {
	f.mu.Lock()
	switch f.state {
	case futureRunning, futureFinished:
		f.mu.Unlock()
		return false
	case futureCancelled, futureCancelledAndNotified:
		f.mu.Unlock()
		return true
	}
	f.state = futureCancelled
	close(f.done)
	f.mu.Unlock()
	return true
}

// Marks the future running, false when it was cancelled before the worker took it
func (f *concurrentFuture) setRunningOrNotifyCancel() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.state {
	case futureCancelled:
		f.state = futureCancelledAndNotified
		return false
	case futurePending:
		f.state = futureRunning
		return true
	}
	panic(&pyRuntimeError{msg: "Future in unexpected state"})
}

// Settles the future with the outcome of its work item
func (f *concurrentFuture) set(result any, err error) {
	f.mu.Lock()
	switch f.state {
	case futureCancelled, futureCancelledAndNotified, futureFinished:
		state := f.state
		f.mu.Unlock()
		panic(&futureInvalidStateError{msg: fmt.Sprintf("%d: %p", state, f)})
	}
	f.result, f.err, f.state = result, err, futureFinished
	close(f.done)
	f.mu.Unlock()
}

// Whether the future was cancelled or finished
func (f *concurrentFuture) isDone() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// Outcome of a done future, the cancelled error for a cancelled one
func (f *concurrentFuture) outcome() (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == futureCancelled || f.state == futureCancelledAndNotified {
		return nil, errFutureCancelled
	}
	return f.result, f.err
}

// Waits up to timeout seconds for the outcome, a negative timeout waiting forever
func (f *concurrentFuture) resultWithin(timeout float64) (any, error) {
	if timeout < 0 {
		<-f.done
		return f.outcome()
	}
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-f.done:
		return f.outcome()
	case <-timer.C:
		return nil, errFutureTimeout
	}
}

// Awaits the future from a task, cancelling it when ctx ends first like a wrapped asyncio future
func (f *concurrentFuture) await(ctx context.Context) (any, error) {
	select {
	case <-f.done:
		return f.outcome()
	case <-ctx.Done():
		f.cancel()
		return nil, ctx.Err()
	}
}

// Thread of an executor pool, a goroutine with a name, an interrupt identity and a join
type executorThread struct {
	name  string
	ident tools.WorkerID
	done  chan struct{}
}

// Thread named name with a fresh interrupt identity, alive until its goroutine closes done
func newExecutorThread(name string) *executorThread {
	return &executorThread{name: name, ident: tools.NewWorkerID(), done: make(chan struct{})}
}

// Waits up to timeout seconds for the thread to exit, a negative timeout waiting forever
func (t *executorThread) join(timeout float64) {
	if timeout < 0 {
		<-t.done
		return
	}
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
	}
}

// Whether the thread's goroutine still runs
func (t *executorThread) isAlive() bool {
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

type executorThreadKey struct{}

// Context of an item running on thread t, the fresh empty context a new thread starts with
func executorThreadContext(t *executorThread) context.Context {
	return context.WithValue(tools.WithWorker(context.Background(), t.ident), executorThreadKey{}, t)
}

// Executor thread running the goroutine that owns ctx, nil outside any pool
func currentExecutorThread(ctx context.Context) *executorThread {
	t, _ := ctx.Value(executorThreadKey{}).(*executorThread)
	return t
}

// Caller context copied onto the executor thread of workerCtx, the copy_context run of hermes
func onExecutorThread(callerCtx, workerCtx context.Context) context.Context {
	ctx := context.WithoutCancel(callerCtx)
	if t := currentExecutorThread(workerCtx); t != nil {
		ctx = context.WithValue(tools.WithWorker(ctx, t.ident), executorThreadKey{}, t)
	}
	return ctx
}

// One submitted call with the future it settles
type executorItem struct {
	fut *concurrentFuture
	fn  func(ctx context.Context) (any, error)
}

// Runs the item on thread t unless it was cancelled first, delivering a panic to its future as an error
func (item *executorItem) run(t *executorThread) {
	if !item.fut.setRunningOrNotifyCancel() {
		return
	}
	result, err := func() (result any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				if e, ok := rec.(error); ok {
					err = e
				} else {
					err = fmt.Errorf("%v", rec)
				}
				result = nil
			}
		}()
		return item.fn(executorThreadContext(t))
	}()
	item.fut.set(result, err)
}
