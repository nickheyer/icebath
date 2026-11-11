package gatewayrun

import (
	"context"
	"fmt"
	"sync"
)

// Bounded pool queueing items for at most maxWorkers threads, the concurrent.futures.ThreadPoolExecutor
type threadPoolExecutor struct {
	maxWorkers   int
	prefix       string
	shutdownLock sync.Mutex
	shut         bool
	all          []*executorThread
	queueMu      sync.Mutex
	queueCond    *sync.Cond
	queue        []*executorItem
	idleMu       sync.Mutex
	idleCount    int
}

// Pool of up to maxWorkers threads named after prefix
func newThreadPoolExecutor(maxWorkers int, prefix string) *threadPoolExecutor {
	if maxWorkers <= 0 {
		panic(&pyValueError{msg: "max_workers must be greater than 0"})
	}
	e := &threadPoolExecutor{maxWorkers: maxWorkers, prefix: prefix}
	e.queueCond = sync.NewCond(&e.queueMu)
	return e
}

// Python ValueError carrying only its message
type pyValueError struct{ msg string }

func (e *pyValueError) Error() string { return e.msg }

// Exception class the error reports, ValueError
func (e *pyValueError) PyTypeName() string { return "ValueError" }

// Appends one queue entry and wakes a waiting thread, nil being the exit sentinel
func (e *threadPoolExecutor) put(item *executorItem) {
	e.queueMu.Lock()
	e.queue = append(e.queue, item)
	e.queueMu.Unlock()
	e.queueCond.Signal()
}

// Next queue entry, blocking until one arrives
func (e *threadPoolExecutor) get() *executorItem {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	for len(e.queue) == 0 {
		e.queueCond.Wait()
	}
	item := e.queue[0]
	e.queue = e.queue[1:]
	return item
}

// Queues the item and starts a thread unless one is idle or the pool is full, refusing after shutdown
func (e *threadPoolExecutor) submit(fn func(ctx context.Context) (any, error)) (*concurrentFuture, error) {
	e.shutdownLock.Lock()
	defer e.shutdownLock.Unlock()
	if e.shut {
		return nil, &pyRuntimeError{msg: "cannot schedule new futures after shutdown"}
	}
	fut := newConcurrentFuture()
	e.put(&executorItem{fut: fut, fn: fn})
	e.adjustThreadCount()
	return fut, nil
}

// Starts one more thread when no idle one can take the queued item and the cap allows, caller holds shutdownLock
func (e *threadPoolExecutor) adjustThreadCount() {
	e.idleMu.Lock()
	if e.idleCount > 0 {
		e.idleCount--
		e.idleMu.Unlock()
		return
	}
	e.idleMu.Unlock()
	if n := len(e.all); n < e.maxWorkers {
		t := newExecutorThread(fmt.Sprintf("%s_%d", e.prefix, n))
		go e.work(t)
		e.all = append(e.all, t)
	}
}

// Runs queued items until the exit sentinel arrives after shutdown, passing the sentinel on
func (e *threadPoolExecutor) work(t *executorThread) {
	defer close(t.done)
	for {
		item := e.get()
		if item != nil {
			item.run(t)
			e.idleMu.Lock()
			e.idleCount++
			e.idleMu.Unlock()
			continue
		}
		e.put(nil)
		return
	}
}

// Refuses new items, cancels queued ones when asked, signals the threads to exit and joins them when wait is set
func (e *threadPoolExecutor) shutdown(wait, cancelFutures bool) {
	e.shutdownLock.Lock()
	e.shut = true
	if cancelFutures {
		e.queueMu.Lock()
		pending := e.queue
		e.queue = nil
		e.queueMu.Unlock()
		for _, item := range pending {
			if item != nil {
				item.fut.cancel()
			}
		}
	}
	e.put(nil)
	threads := append([]*executorThread(nil), e.all...)
	e.shutdownLock.Unlock()
	if wait {
		for _, t := range threads {
			t.join(-1)
		}
	}
}

// Whether shutdown ran
func (e *threadPoolExecutor) isShutdown() bool {
	e.shutdownLock.Lock()
	defer e.shutdownLock.Unlock()
	return e.shut
}

// Every thread the pool started
func (e *threadPoolExecutor) threads() []executorWorker {
	e.shutdownLock.Lock()
	defer e.shutdownLock.Unlock()
	out := make([]executorWorker, 0, len(e.all))
	for _, t := range e.all {
		out = append(out, t)
	}
	return out
}
