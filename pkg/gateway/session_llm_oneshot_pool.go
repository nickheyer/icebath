package gateway

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Model calls one owner runs at once and calls it admits in all, running and queued
const (
	oneshotWorkers  = 4
	oneshotAdmitted = 16
)

// One admitted one shot call, its result once a worker ran it or its cancellation while queued
type oneshotCall struct {
	mu        sync.Mutex
	done      chan struct{}
	running   bool
	cancelled bool
	result    any
	err       error
	onDone    []func(*oneshotCall)
}

// Runs the done callbacks once the call settled, outside its lock
func (c *oneshotCall) settled() {
	c.mu.Lock()
	callbacks := c.onDone
	c.onDone = nil
	c.mu.Unlock()
	for _, callback := range callbacks {
		callback(c)
	}
}

// Calls fn when the call settles, at once when it already has
func (c *oneshotCall) addDoneCallback(fn func(*oneshotCall)) {
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		fn(c)
		return
	default:
	}
	c.onDone = append(c.onDone, fn)
	c.mu.Unlock()
}

// Cancels a call still queued, false once it started running or finished
func (c *oneshotCall) cancel() bool {
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return c.cancelled
	default:
	}
	if c.running {
		c.mu.Unlock()
		return false
	}
	c.cancelled = true
	close(c.done)
	c.mu.Unlock()
	c.settled()
	return true
}

// Marks the call running, false when it was cancelled first
func (c *oneshotCall) start() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelled {
		return false
	}
	c.running = true
	return true
}

// Settles a running call with its result
func (c *oneshotCall) finish(result any, err error) {
	c.mu.Lock()
	c.result, c.err = result, err
	close(c.done)
	c.mu.Unlock()
	c.settled()
}

// Serial owner goroutine the answers and the stop run on, started on demand and gone while idle
type ownerLoop struct {
	mu      sync.Mutex
	jobs    []func()
	running bool
}

// Queues a job behind every job posted before it
func (l *ownerLoop) post(job func()) {
	l.mu.Lock()
	l.jobs = append(l.jobs, job)
	if l.running {
		l.mu.Unlock()
		return
	}
	l.running = true
	l.mu.Unlock()
	go l.run()
}

// Runs posted jobs in order until none is left
func (l *ownerLoop) run() {
	for {
		l.mu.Lock()
		if len(l.jobs) == 0 {
			l.running = false
			l.mu.Unlock()
			return
		}
		job := l.jobs[0]
		l.jobs = l.jobs[1:]
		l.mu.Unlock()
		job()
	}
}

// Runs a job on the loop and waits for it, never from inside a loop job
func (l *ownerLoop) call(job func()) {
	done := make(chan struct{})
	l.post(func() {
		defer close(done)
		job()
	})
	<-done
}

// One owner's one shot threads and admitted calls, its own FIFO so a cancelled call leaves the queue at once
type oneshotPool struct {
	lock     sync.Mutex
	calls    map[*oneshotCall]bool
	queue    []*oneshotCall
	work     map[*oneshotCall]func() (any, error)
	threads  int
	closed   bool
	stopping *pythread.Event
	answers  map[*oneshotAnswer]bool
	executor *tools.DaemonPool
	loop     *ownerLoop
}

// Empty pool whose workers start on its own executor and whose answers run on its owner loop
func newOneshotPool() *oneshotPool {
	return &oneshotPool{calls: map[*oneshotCall]bool{}, work: map[*oneshotCall]func() (any, error){},
		stopping: &pythread.Event{}, answers: map[*oneshotAnswer]bool{},
		executor: tools.NewDaemonPool(oneshotWorkers, "icebath-oneshot"), loop: &ownerLoop{}}
}

// Pool of the authority, created on first use
func oneshotPoolOf(a *SessionAuthority) *oneshotPool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.oneshotPool == nil {
		a.oneshotPool = newOneshotPool()
	}
	return a.oneshotPool
}

// Admits work or refuses it as runtime_draining or oneshot_busy, returning its call
func (p *oneshotPool) submit(work func() (any, error)) (*oneshotCall, error) {
	call := &oneshotCall{done: make(chan struct{})}
	p.lock.Lock()
	if p.closed {
		p.lock.Unlock()
		return nil, state.NewRuntimeStoreError("runtime_draining", nil, nil)
	}
	if len(p.calls) >= oneshotAdmitted {
		p.lock.Unlock()
		detail := fmt.Sprintf("%d one-shot generations already in flight", oneshotAdmitted)
		return nil, state.NewRuntimeStoreError("oneshot_busy", nil, &detail)
	}
	p.calls[call] = true
	p.queue = append(p.queue, call)
	p.work[call] = work
	spawn := p.threads < oneshotWorkers
	if spawn {
		p.threads++
	}
	p.lock.Unlock()
	call.addDoneCallback(p.release)
	if spawn {
		_, err := p.executor.Submit(context.Background(), func(context.Context) (any, error) {
			p.drain()
			return nil, nil
		})
		if err != nil {
			p.lock.Lock()
			p.threads--
			orphaned := p.threads == 0
			p.lock.Unlock()
			if orphaned && call.cancel() {
				return nil, &PyError{Kind: "RuntimeError", Msg: err.Error()}
			}
		}
	}
	return call, nil
}

// Drops a finished or cancelled call's slot and its queued work
func (p *oneshotPool) release(call *oneshotCall) {
	p.lock.Lock()
	delete(p.calls, call)
	p.dequeue(call)
	p.lock.Unlock()
}

// Removes the call from the queue, the caller holding the lock
func (p *oneshotPool) dequeue(call *oneshotCall) {
	for i, queued := range p.queue {
		if queued == call {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			break
		}
	}
	delete(p.work, call)
}

// Runs queued calls oldest first until the queue is empty, then ends the thread
func (p *oneshotPool) drain() {
	for {
		p.lock.Lock()
		if len(p.queue) == 0 {
			p.threads--
			p.lock.Unlock()
			return
		}
		call := p.queue[0]
		work := p.work[call]
		p.dequeue(call)
		p.lock.Unlock()
		if call.start() {
			result, err := func() (result any, err error) {
				defer func() {
					if rec := recover(); rec != nil {
						err = fmt.Errorf("%v", rec)
					}
				}()
				return work()
			}()
			call.finish(result, err)
		}
	}
}

// Owner shutdown on the owner loop refusing new calls, cancelling queued ones and signalling each outstanding answer
func StopOneshots(a *SessionAuthority) {
	pool := oneshotPoolOf(a)
	pool.loop.call(pool.stop)
}

// Refuses new calls, sets stopping and cancels every queued call, run on the owner loop
func (p *oneshotPool) stop() {
	p.lock.Lock()
	p.closed = true
	queued := append([]*oneshotCall(nil), p.queue...)
	p.lock.Unlock()
	p.stopping.Set()
	for _, call := range queued {
		call.cancel()
	}
}
