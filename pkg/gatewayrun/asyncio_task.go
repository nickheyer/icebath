package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Error a cancelled task or future settles with, the asyncio.CancelledError of hermes
var errTaskCancelled = errors.New("task cancelled")

// Settable result with completion callbacks, the asyncio.Future of hermes
type gatewayFuture struct {
	mu        sync.Mutex
	done      chan struct{}
	settled   bool
	result    any
	err       error
	callbacks []func(*gatewayFuture)
}

// Pending future
func newGatewayFuture() *gatewayFuture {
	return &gatewayFuture{done: make(chan struct{})}
}

// Settles the future once with result and err, false when it was already settled
func (f *gatewayFuture) settle(result any, err error) bool {
	f.mu.Lock()
	if f.settled {
		f.mu.Unlock()
		return false
	}
	f.settled, f.result, f.err = true, result, err
	callbacks := f.callbacks
	f.callbacks = nil
	close(f.done)
	f.mu.Unlock()
	for _, fn := range callbacks {
		fn(f)
	}
	return true
}

// Whether the future has settled
func (f *gatewayFuture) isDone() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// Whether the future settled by cancellation
func (f *gatewayFuture) cancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settled && errors.Is(f.err, errTaskCancelled)
}

// Runs fn once the future settles, at once for a settled future
func (f *gatewayFuture) addDoneCallback(fn func(*gatewayFuture)) {
	f.mu.Lock()
	if f.settled {
		f.mu.Unlock()
		fn(f)
		return
	}
	f.callbacks = append(f.callbacks, fn)
	f.mu.Unlock()
}

// Blocks until the future settles or ctx ends, returning its result
func (f *gatewayFuture) wait(ctx context.Context) (any, error) {
	select {
	case <-f.done:
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.result, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Goroutine with cancellation and a future of its outcome, the asyncio.Task of hermes
type gatewayTask struct {
	*gatewayFuture
	name   string
	cancel context.CancelFunc
}

type gatewayTaskKey struct{}

// Starts fn on its own goroutine under a cancellable child of ctx
func spawnGatewayTask(ctx context.Context, name string, fn func(ctx context.Context) (any, error)) *gatewayTask {
	taskCtx, cancel := context.WithCancel(ctx)
	task := &gatewayTask{gatewayFuture: newGatewayFuture(), name: name, cancel: cancel}
	taskCtx = context.WithValue(taskCtx, gatewayTaskKey{}, task)
	go func() {
		defer cancel()
		result, err := runTaskBody(taskCtx, name, fn)
		if errors.Is(err, context.Canceled) {
			err = errTaskCancelled
		}
		task.settle(result, err)
	}()
	return task
}

// Loop whose exception handler reports a task that crashed, the running gateway loop and nil outside a started gateway
var taskExceptionLoop atomic.Pointer[gateway.CallbackLoop]

// Runs a task body, a panic becoming the task exception and reported to the loop handler so the process keeps running
func runTaskBody(ctx context.Context, name string, fn func(ctx context.Context) (any, error)) (result any, err error) {
	defer func() {
		crash := recover()
		if crash == nil {
			return
		}
		failure, isError := crash.(error)
		if !isError {
			failure = fmt.Errorf("%v", crash)
		}
		result, err = nil, failure
		taskExceptionLoop.Load().CallExceptionHandler(gateway.LoopExceptionContext{
			Message: "Task exception was never retrieved", Exception: failure, Task: name})
	}()
	return fn(ctx)
}

// Task running the goroutine that owns ctx, nil outside any task
func currentGatewayTask(ctx context.Context) *gatewayTask {
	task, _ := ctx.Value(gatewayTaskKey{}).(*gatewayTask)
	return task
}

// Requests cancellation, false when the task already finished
func (t *gatewayTask) Cancel() bool {
	if t.isDone() {
		return false
	}
	t.cancel()
	return true
}
