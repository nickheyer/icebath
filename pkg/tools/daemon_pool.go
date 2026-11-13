package tools

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Bounded worker pool whose workers never hold the process open, each task running in its caller's context

// ErrPoolShutdown is returned by Submit once the pool was shut down
var ErrPoolShutdown = errors.New("pool is shut down")

// ErrFutureCancelled is returned by a future that was cancelled before it ran
var ErrFutureCancelled = errors.New("future cancelled")

// PoolFuture is the pending result of a submitted task
type PoolFuture struct {
	done      chan struct{}
	mu        sync.Mutex
	result    any
	err       error
	cancelled bool
	running   bool
}

// Done is closed once the task finished or was cancelled
func (f *PoolFuture) Done() <-chan struct{} { return f.done }

// Result waits up to timeout for the outcome, zero waits forever
func (f *PoolFuture) Result(timeout time.Duration) (any, error) {
	if timeout <= 0 {
		<-f.done
	} else {
		select {
		case <-f.done:
		case <-time.After(timeout):
			return nil, context.DeadlineExceeded
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result, f.err
}

// Cancel refuses a task that has not started, false once it is running or finished
func (f *PoolFuture) Cancel() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running || f.cancelled {
		return f.cancelled
	}
	select {
	case <-f.done:
		return false
	default:
	}
	f.cancelled = true
	f.err = ErrFutureCancelled
	close(f.done)
	return true
}

// Cancelled reports whether Cancel won
func (f *PoolFuture) Cancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled
}

type poolTask struct {
	ctx    context.Context
	fn     func(ctx context.Context) (any, error)
	future *PoolFuture
}

// DaemonPool is the shared executor for background work that must never block process exit
type DaemonPool struct {
	mu         sync.Mutex
	maxWorkers int
	prefix     string
	workers    int
	idle       int
	queue      []*poolTask
	wake       chan struct{}
	shutdown   bool
}

// NewDaemonPool builds a pool of at most maxWorkers workers named with prefix
func NewDaemonPool(maxWorkers int, prefix string) *DaemonPool {
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	return &DaemonPool{maxWorkers: maxWorkers, prefix: prefix, wake: make(chan struct{}, 1)}
}

// Submit queues fn to run on a worker in the caller's context and returns its future
func (p *DaemonPool) Submit(ctx context.Context, fn func(ctx context.Context) (any, error)) (*PoolFuture, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	future := &PoolFuture{done: make(chan struct{})}
	p.mu.Lock()
	if p.shutdown {
		p.mu.Unlock()
		return nil, ErrPoolShutdown
	}
	p.queue = append(p.queue, &poolTask{ctx: ctx, fn: fn, future: future})
	p.adjustWorkerCountLocked()
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return future, nil
}

// Starts a worker when none is idle and the cap allows, caller holds the lock
func (p *DaemonPool) adjustWorkerCountLocked() {
	if p.idle > 0 {
		p.idle--
		return
	}
	if p.workers < p.maxWorkers {
		p.workers++
		go p.worker(fmt.Sprintf("%s_%d", p.prefix, p.workers-1))
	}
}

// Runs queued tasks until shutdown, parking idle between them
func (p *DaemonPool) worker(name string) {
	for {
		p.mu.Lock()
		for len(p.queue) == 0 {
			if p.shutdown {
				p.workers--
				p.mu.Unlock()
				return
			}
			p.idle++
			p.mu.Unlock()
			<-p.wake
			p.mu.Lock()
			if p.idle > 0 {
				p.idle--
			}
		}
		task := p.queue[0]
		p.queue = p.queue[1:]
		more := len(p.queue) > 0
		p.mu.Unlock()
		if more {
			select {
			case p.wake <- struct{}{}:
			default:
			}
		}
		p.run(task)
	}
}

// Executes one task, recording a panic as its error
func (p *DaemonPool) run(t *poolTask) {
	f := t.future
	f.mu.Lock()
	if f.cancelled {
		f.mu.Unlock()
		return
	}
	f.running = true
	f.mu.Unlock()
	var result any
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("task panicked: %v", r)
			}
		}()
		result, err = t.fn(t.ctx)
	}()
	f.mu.Lock()
	f.result, f.err = result, err
	f.mu.Unlock()
	close(f.done)
}

// Shutdown refuses new work, cancelling queued tasks when cancelFutures, and waits for running ones when wait
func (p *DaemonPool) Shutdown(wait, cancelFutures bool) {
	p.mu.Lock()
	p.shutdown = true
	var pending []*poolTask
	if cancelFutures {
		pending = p.queue
		p.queue = nil
	}
	remaining := append([]*poolTask{}, p.queue...)
	p.mu.Unlock()
	for _, t := range pending {
		t.future.Cancel()
	}
	for i := 0; i < p.maxWorkers; i++ {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
	if !wait {
		return
	}
	for _, t := range remaining {
		<-t.future.done
	}
	deadline := time.Now().Add(time.Minute)
	for {
		p.mu.Lock()
		busy := p.workers - p.idle
		queued := len(p.queue)
		p.mu.Unlock()
		if busy == 0 && queued == 0 || time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}
