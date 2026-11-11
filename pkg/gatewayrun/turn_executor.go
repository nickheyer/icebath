package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Seconds a parked worker waits for work before its thread exits
const turnExecutorIdleTimeoutS = 60.0

// Worker of a pool that shutdown joins with a bounded wait
type executorWorker interface {
	join(timeout float64)
	isAlive() bool
}

// Pool the runner submits blocking work to, the concurrent.futures executor of hermes
type gatewayExecutor interface {
	submit(fn func(ctx context.Context) (any, error)) (*concurrentFuture, error)
	shutdown(wait, cancelFutures bool)
	isShutdown() bool
	threads() []executorWorker
}

// Parked or busy thread of the unbounded executor with the inbox its next item arrives on
type turnWorker struct {
	thread *executorThread
	inbox  chan *executorItem
}

// Starts every item at once on a parked worker or a new thread, with no queue and no cap
type unboundedThreadExecutor struct {
	prefix      string
	busy        map[*executorThread]struct{}
	idle        []*turnWorker
	shut        bool
	lock        sync.Mutex
	n           int
	idleTimeout float64
}

// Executor naming its threads after prefix whose parked workers exit after idleTimeout seconds
func newUnboundedThreadExecutor(prefix string, idleTimeout float64) *unboundedThreadExecutor {
	return &unboundedThreadExecutor{prefix: prefix, busy: map[*executorThread]struct{}{}, idleTimeout: idleTimeout}
}

// Hands the item to the warmest parked worker, else starts a thread, refusing after shutdown
func (e *unboundedThreadExecutor) submit(fn func(ctx context.Context) (any, error)) (*concurrentFuture, error) {
	fut := newConcurrentFuture()
	item := &executorItem{fut: fut, fn: fn}
	e.lock.Lock()
	defer e.lock.Unlock()
	if e.shut {
		return nil, &pyRuntimeError{msg: "cannot schedule new futures after shutdown"}
	}
	if n := len(e.idle); n > 0 {
		worker := e.idle[n-1]
		e.idle = e.idle[:n-1]
		e.busy[worker.thread] = struct{}{}
		worker.inbox <- item
		return fut, nil
	}
	e.n++
	worker := &turnWorker{thread: newExecutorThread(fmt.Sprintf("%s_%d", e.prefix, e.n)), inbox: make(chan *executorItem, 1)}
	go e.work(worker, item)
	e.busy[worker.thread] = struct{}{}
	return fut, nil
}

// Runs items until a park returns none, then ends the thread
func (e *unboundedThreadExecutor) work(worker *turnWorker, item *executorItem) {
	defer close(worker.thread.done)
	for item != nil {
		item.run(worker.thread)
		item = e.park(worker)
	}
}

// Resets the thread and waits for its next item, nil when it must exit
func (e *unboundedThreadExecutor) park(worker *turnWorker) *executorItem {
	reusable := func() (ok bool) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Default().With("logger", "gateway.turn_executor").Warn("turn worker state reset failed; retiring the thread", "error", rec)
				ok = false
			}
		}()
		resetReusedThreadState(worker.thread.ident)
		return true
	}()
	e.lock.Lock()
	delete(e.busy, worker.thread)
	if e.shut || !reusable {
		e.lock.Unlock()
		return nil
	}
	e.idle = append(e.idle, worker)
	e.lock.Unlock()
	timer := time.NewTimer(time.Duration(e.idleTimeout * float64(time.Second)))
	defer timer.Stop()
	select {
	case item := <-worker.inbox:
		return item
	case <-timer.C:
	}
	e.lock.Lock()
	if i := slices.Index(e.idle, worker); i >= 0 {
		e.idle = slices.Delete(e.idle, i, i+1)
		e.lock.Unlock()
		return nil
	}
	e.lock.Unlock()
	return <-worker.inbox
}

// Refuses new items, wakes parked workers to exit and joins the busy threads when wait is set
func (e *unboundedThreadExecutor) shutdown(wait, _ bool) {
	e.lock.Lock()
	e.shut = true
	busy := make([]*executorThread, 0, len(e.busy))
	for t := range e.busy {
		busy = append(busy, t)
	}
	idle := e.idle
	e.idle = nil
	e.lock.Unlock()
	for _, worker := range idle {
		worker.inbox <- nil
	}
	if wait {
		for _, t := range busy {
			t.join(-1)
		}
	}
}

// Whether shutdown ran
func (e *unboundedThreadExecutor) isShutdown() bool {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.shut
}

// Threads running an item now
func (e *unboundedThreadExecutor) threads() []executorWorker {
	e.lock.Lock()
	defer e.lock.Unlock()
	out := make([]executorWorker, 0, len(e.busy))
	for t := range e.busy {
		out = append(out, t)
	}
	return out
}
