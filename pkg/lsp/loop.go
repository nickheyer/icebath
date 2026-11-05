package lsp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Cooperative scheduler of one service, every task holding mu while it runs and releasing it across blocking waits
type loop struct {
	mu      sync.Mutex
	tasks   sync.WaitGroup
	stateMu sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	started bool
	stopped bool
}

func newLoop() *loop {
	ctx, cancel := context.WithCancel(context.Background())
	return &loop{ctx: ctx, cancel: cancel}
}

// Marks the loop as accepting tasks
func (l *loop) start() {
	l.stateMu.Lock()
	l.started = true
	l.stateMu.Unlock()
}

// Whether the loop accepts tasks
func (l *loop) running() bool {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	return l.started && !l.stopped
}

// Stops accepting tasks, cancels every running one and joins them for up to two seconds
func (l *loop) stop() {
	l.stateMu.Lock()
	l.stopped = true
	l.stateMu.Unlock()
	l.cancel()
	joined := make(chan struct{})
	go func() {
		l.tasks.Wait()
		close(joined)
	}()
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	select {
	case <-joined:
	case <-t.C:
	}
}

// One scheduled task and its completion
type task struct {
	done   chan struct{}
	cancel context.CancelFunc
}

// Whether the task finished
func (t *task) finished() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Starts fn as a task under a child of parent that also ends when the loop stops
func (l *loop) spawn(parent context.Context, fn func(ctx context.Context)) *task {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(l.ctx, cancel)
	t := &task{done: make(chan struct{}), cancel: cancel}
	l.tasks.Add(1)
	go func() {
		defer l.tasks.Done()
		defer close(t.done)
		defer stop()
		defer cancel()
		l.mu.Lock()
		defer l.mu.Unlock()
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("Task exception was never retrieved", "logger", "asyncio", "exception", fmt.Sprint(r))
			}
		}()
		fn(ctx)
	}()
	return t
}

// Releases the loop around a blocking call made from a task
func (l *loop) unlocked(fn func()) {
	l.mu.Unlock()
	defer l.mu.Lock()
	fn()
}

// Runs fn holding the loop from a goroutine that is not a task
func (l *loop) with(fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fn()
}

// Sleeps inside a task, the cancellation error when the task is cancelled first
func (l *loop) sleep(ctx context.Context, d time.Duration) error {
	var err error
	l.unlocked(func() {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	return err
}

// Waits inside a task for a channel to close, the cancellation error when the task is cancelled first
func (l *loop) await(ctx context.Context, ch <-chan struct{}) error {
	var err error
	l.unlocked(func() {
		select {
		case <-ch:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	return err
}

// Waits inside a task for the first of several channels within a timeout, false on timeout
func (l *loop) awaitAny(ctx context.Context, timeout time.Duration, chans ...<-chan struct{}) (bool, error) {
	var woke bool
	var err error
	l.unlocked(func() {
		t := time.NewTimer(timeout)
		defer t.Stop()
		cases := []reflect.SelectCase{
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(t.C)},
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
		}
		for _, ch := range chans {
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)})
		}
		switch chosen, _, _ := reflect.Select(cases); chosen {
		case 0:
		case 1:
			err = ctx.Err()
		default:
			woke = true
		}
	})
	return woke, err
}

// Whether an error is a task cancellation or an expired deadline delivered as one
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Runs fn under a deadline the way asyncio.wait_for does, a cancellation of the caller passing through
func waitFor[T any](ctx context.Context, d time.Duration, fn func(ctx context.Context) (T, error)) (T, error) {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	v, err := fn(cctx)
	if err != nil && ctx.Err() == nil && cctx.Err() != nil && isCancellation(err) {
		return v, &TimeoutError{}
	}
	return v, err
}

// Schedules fn on the loop from outside it and blocks for its result, cancelling it after timeout
func runOnLoop[T any](l *loop, timeout time.Duration, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if !l.running() {
		return zero, errors.New("background loop not running")
	}
	var v T
	var err error
	t := l.spawn(context.Background(), func(ctx context.Context) {
		defer func() {
			if r := recover(); r != nil {
				err = panicError(r)
			}
		}()
		v, err = fn(ctx)
	})
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.done:
		return v, err
	case <-timer.C:
		t.cancel()
		return zero, &TimeoutError{}
	}
}

// Error carried by a recovered panic
func panicError(r any) error {
	if e, ok := r.(error); ok {
		return e
	}
	return &pyExc{typ: "RuntimeError", text: errorString(r)}
}

func errorString(r any) string {
	if s, ok := r.(string); ok {
		return s
	}
	return pyrepr.Str(r)
}

// Future resolved once from a task, waited on from tasks or outside the loop
type future[T any] struct {
	done      chan struct{}
	once      sync.Once
	value     T
	err       error
	cancelled bool
}

func newFuture[T any]() *future[T] { return &future[T]{done: make(chan struct{})} }

// Resolves the future with a result, ignored when already resolved
func (f *future[T]) setResult(v T) {
	f.once.Do(func() { f.value = v; close(f.done) })
}

// Resolves the future with an error, ignored when already resolved
func (f *future[T]) setError(err error) {
	f.once.Do(func() { f.err = err; close(f.done) })
}

// Resolves the future as cancelled, ignored when already resolved
func (f *future[T]) setCancelled() {
	f.once.Do(func() { f.cancelled = true; close(f.done) })
}

// Whether the future resolved
func (f *future[T]) isDone() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// Sticky event that waiters clear before waiting, as asyncio.Event is used here
type event struct {
	set bool
	ch  chan struct{}
}

func newEvent() *event { return &event{ch: make(chan struct{})} }

// Wakes every waiter and stays set until cleared
func (e *event) fire() {
	if !e.set {
		e.set = true
		close(e.ch)
	}
}

// Resets the event so the next wait blocks until the next fire
func (e *event) clear() {
	if e.set {
		e.set = false
		e.ch = make(chan struct{})
	}
}
