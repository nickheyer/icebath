package cron

import (
	"container/heap"
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	periodicThreadName         = "icebath-periodic-scheduler"
	periodicCallbackThreadName = "icebath-periodic-callback"
)

// ScheduledHandle is the cancel token for one scheduled periodic callback
type ScheduledHandle struct {
	scheduler *PeriodicScheduler
	fn        func(ctx context.Context) bool
	ctx       context.Context
	interval  time.Duration
	cancelled bool
	runner    chan struct{}
	runnerGID uint64
}

// Reports whether the handle was cancelled or stopped itself
func (h *ScheduledHandle) Cancelled() bool {
	h.scheduler.mu.Lock()
	defer h.scheduler.mu.Unlock()
	return h.cancelled
}

// Stops future runs and waits up to wait for an in-flight run of this callback to finish
func (h *ScheduledHandle) Cancel(wait time.Duration) {
	h.scheduler.cancel(h, wait)
}

type periodicEntry struct {
	due    time.Time
	seq    uint64
	handle *ScheduledHandle
}

type periodicHeap []periodicEntry

func (q periodicHeap) Len() int { return len(q) }
func (q periodicHeap) Less(i, j int) bool {
	if !q[i].due.Equal(q[j].due) {
		return q[i].due.Before(q[j].due)
	}
	return q[i].seq < q[j].seq
}
func (q periodicHeap) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *periodicHeap) Push(x any)   { *q = append(*q, x.(periodicEntry)) }
func (q *periodicHeap) Pop() any {
	old := *q
	e := old[len(old)-1]
	*q = old[:len(old)-1]
	return e
}

// PeriodicScheduler orders due times on one goroutine and runs every due body on its own worker
type PeriodicScheduler struct {
	mu          sync.Mutex
	wake        chan struct{}
	heap        periodicHeap
	seq         uint64
	started     bool
	startWorker func(run func()) error
}

// Returns a scheduler whose loop goroutine starts with the first scheduled callback
func NewPeriodicScheduler() *PeriodicScheduler {
	return &PeriodicScheduler{
		wake: make(chan struct{}, 1),
		startWorker: func(run func()) error {
			go run()
			return nil
		},
	}
}

// Runs fn in ctx every interval after the previous run returned until fn returns false or the handle is cancelled
func (p *PeriodicScheduler) Schedule(ctx context.Context, fn func(ctx context.Context) bool, interval time.Duration) *ScheduledHandle {
	h := &ScheduledHandle{scheduler: p, fn: fn, ctx: ctx, interval: interval}
	p.mu.Lock()
	p.requeue(h)
	if !p.started {
		p.started = true
		go pprof.Do(context.Background(), pprof.Labels("thread", periodicThreadName), func(context.Context) { p.run() })
	}
	p.notify()
	p.mu.Unlock()
	return h
}

func (p *PeriodicScheduler) notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *PeriodicScheduler) cancel(h *ScheduledHandle, wait time.Duration) {
	p.mu.Lock()
	retirePeriodic(h)
	p.notify()
	runner, runnerGID := h.runner, h.runnerGID
	p.mu.Unlock()
	if wait > 0 && runner != nil && runnerGID != currentGoroutineID() {
		select {
		case <-runner:
		case <-time.After(wait):
		}
	}
}

// Starts the handle body on its own worker and requeues the handle when the worker cannot start
func (p *PeriodicScheduler) dispatch(h *ScheduledHandle) {
	done := make(chan struct{})
	h.runner = done
	fn, ctx := h.fn, h.ctx
	label := fmt.Sprintf("%s-%p", periodicCallbackThreadName, h)
	err := p.startWorker(func() {
		pprof.Do(ctx, pprof.Labels("thread", label), func(lctx context.Context) {
			p.runCallback(h, fn, lctx, done)
		})
	})
	if err != nil {
		h.runner = nil
		slog.Warn(fmt.Sprintf("failed to start periodic callback worker %p; retrying in %v s", fn, h.interval.Seconds()), "error", err)
		if !h.cancelled {
			p.requeue(h)
			p.notify()
		}
	}
}

// Marks the handle cancelled and drops its callback and context so the owner is released
func retirePeriodic(h *ScheduledHandle) {
	h.cancelled = true
	h.fn = nil
	h.ctx = nil
}

func (p *PeriodicScheduler) requeue(h *ScheduledHandle) {
	p.seq++
	heap.Push(&p.heap, periodicEntry{due: time.Now().Add(h.interval), seq: p.seq, handle: h})
}

// Runs one body, logging a panic at debug, then retires or requeues the handle
func (p *PeriodicScheduler) runCallback(h *ScheduledHandle, fn func(ctx context.Context) bool, ctx context.Context, done chan struct{}) {
	stop := false
	p.mu.Lock()
	h.runnerGID = currentGoroutineID()
	p.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("periodic callback %p raised", fn), "error", r)
		}
		p.mu.Lock()
		h.runner = nil
		h.runnerGID = 0
		if stop {
			retirePeriodic(h)
		} else if !h.cancelled {
			p.requeue(h)
		}
		p.notify()
		p.mu.Unlock()
		close(done)
	}()
	stop = !fn(ctx)
}

func (p *PeriodicScheduler) run() {
	for {
		p.mu.Lock()
		if len(p.heap) == 0 {
			p.mu.Unlock()
			<-p.wake
			continue
		}
		top := p.heap[0]
		if top.handle.cancelled {
			heap.Pop(&p.heap)
			p.mu.Unlock()
			continue
		}
		delay := time.Until(top.due)
		if delay > 0 {
			p.mu.Unlock()
			timer := time.NewTimer(delay)
			select {
			case <-p.wake:
			case <-timer.C:
			}
			timer.Stop()
			continue
		}
		heap.Pop(&p.heap)
		p.dispatch(top.handle)
		p.mu.Unlock()
	}
}

// Parses the running goroutine number from its stack header so a callback never waits on itself
func currentGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(strings.TrimPrefix(string(buf[:n]), "goroutine "))
	if len(fields) == 0 {
		return 0
	}
	id, _ := strconv.ParseUint(fields[0], 10, 64)
	return id
}

var periodicDefault = NewPeriodicScheduler()

// Runs fn every interval on the shared process wide scheduler
func SchedulePeriodic(ctx context.Context, fn func(ctx context.Context) bool, interval time.Duration) *ScheduledHandle {
	return periodicDefault.Schedule(ctx, fn, interval)
}
