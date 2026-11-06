package monitoring

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"runtime/pprof"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	maxQueue   = 10_000
	drainBatch = 256
)

// Subscriber receives dispatched batches on the dispatcher goroutine and is matched by identity
type Subscriber struct {
	fn func(batch []*pyjson.Object)
}

// Subscriber handle around a batch callback
func NewSubscriber(fn func(batch []*pyjson.Object)) *Subscriber { return &Subscriber{fn: fn} }

// EmitterStats is the queued, dispatched, dropped and subscriber count snapshot
type EmitterStats struct {
	Queued, Dispatched, Dropped, Subscribers int
}

// MonitoringEmitter owns the bounded queue, the dispatcher goroutine and the subscriber list
type MonitoringEmitter struct {
	enabled     atomic.Bool
	mu          sync.Mutex
	idle        *sync.Cond
	q           []*pyjson.Object
	unfinished  int
	notify      chan struct{}
	dropped     atomic.Int64
	dispatched  atomic.Int64
	stop        chan struct{}
	stopOnce    sync.Once
	startMu     sync.Mutex
	started     atomic.Bool
	done        chan struct{}
	subMu       sync.Mutex
	subscribers []*Subscriber
}

// Emitter that collects only while enabled
func NewMonitoringEmitter(enabled bool) *MonitoringEmitter {
	e := &MonitoringEmitter{notify: make(chan struct{}, 1), stop: make(chan struct{})}
	e.idle = sync.NewCond(&e.mu)
	e.enabled.Store(enabled)
	return e
}

// Dict payload of an event, an ordered object or a plain map with sorted keys
func eventPayload(event any) (*pyjson.Object, error) {
	switch ev := event.(type) {
	case Event:
		return ev.ToDict(), nil
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range ev.Keys {
			out.Set(k, ev.Vals[k])
		}
		return out, nil
	case map[string]any:
		keys := make([]string, 0, len(ev))
		for k := range ev {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := pyjson.NewObject()
		for _, k := range keys {
			out.Set(k, ev[k])
		}
		return out, nil
	}
	return nil, fmt.Errorf("cannot convert %T to dict", event)
}

// Enqueues an event without blocking or panicking, dropping the oldest entry when the queue is full
func (e *MonitoringEmitter) Emit(event any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("monitoring emit failed: %v", r))
		}
	}()
	if !e.enabled.Load() {
		return
	}
	payload, err := eventPayload(event)
	if err != nil {
		slog.Debug(fmt.Sprintf("monitoring emit failed: %v", err))
		return
	}
	if _, ok := payload.Get("ts_ns"); !ok {
		payload.Set("ts_ns", time.Now().UnixNano())
	}
	e.ensureStarted()
	e.mu.Lock()
	if len(e.q) >= maxQueue {
		e.q[0] = nil
		e.q = e.q[1:]
		e.unfinished--
		e.dropped.Add(1)
	}
	e.q = append(e.q, payload)
	e.unfinished++
	e.mu.Unlock()
	select {
	case e.notify <- struct{}{}:
	default:
	}
}

// Starts the dispatcher goroutine once per start cycle
func (e *MonitoringEmitter) ensureStarted() {
	if e.started.Load() {
		return
	}
	e.startMu.Lock()
	defer e.startMu.Unlock()
	if e.started.Load() {
		return
	}
	done := make(chan struct{})
	e.done = done
	go pprof.Do(context.Background(), pprof.Labels("name", "icebath-monitoring-dispatch"), func(context.Context) {
		defer close(done)
		e.run()
	})
	e.started.Store(true)
}

// Takes up to one drain batch from the queue head
func (e *MonitoringEmitter) take() []*pyjson.Object {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := min(len(e.q), drainBatch)
	batch := append([]*pyjson.Object(nil), e.q[:n]...)
	clear(e.q[:n])
	e.q = e.q[n:]
	return batch
}

// Drains batches to subscribers until stopped, polling every half second while idle
func (e *MonitoringEmitter) run() {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-e.stop:
			return
		default:
		}
		batch := e.take()
		if len(batch) == 0 {
			timer.Reset(500 * time.Millisecond)
			select {
			case <-e.stop:
				return
			case <-e.notify:
			case <-timer.C:
			}
			continue
		}
		func() {
			defer func() {
				e.mu.Lock()
				e.unfinished -= len(batch)
				if e.unfinished <= 0 {
					e.unfinished = 0
					e.idle.Broadcast()
				}
				e.mu.Unlock()
			}()
			e.dispatch(batch)
		}()
	}
}

// Fans one batch out to every subscriber, isolating each failure
func (e *MonitoringEmitter) dispatch(batch []*pyjson.Object) {
	e.subMu.Lock()
	subs := append([]*Subscriber(nil), e.subscribers...)
	e.subMu.Unlock()
	for _, sub := range subs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Debug(fmt.Sprintf("monitoring subscriber failed: %v", r))
				}
			}()
			sub.fn(batch)
		}()
	}
	e.dispatched.Add(int64(len(batch)))
}

// Registers a live batch subscriber once and enables collection
func (e *MonitoringEmitter) Subscribe(cb *Subscriber) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	found := false
	for _, s := range e.subscribers {
		if s == cb {
			found = true
			break
		}
	}
	if !found {
		e.subscribers = append(e.subscribers, cb)
	}
	e.enabled.Store(true)
}

// Removes the first registration of a subscriber and disables collection when none remain
func (e *MonitoringEmitter) Unsubscribe(cb *Subscriber) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	for i, s := range e.subscribers {
		if s == cb {
			e.subscribers = append(e.subscribers[:i:i], e.subscribers[i+1:]...)
			break
		}
	}
	if len(e.subscribers) == 0 {
		e.enabled.Store(false)
	}
}

// Waits up to timeout seconds for queued and in flight batches to finish dispatch
func (e *MonitoringEmitter) Flush(timeout float64) {
	if timeout <= 0 {
		return
	}
	if math.IsNaN(timeout) {
		panic("ValueError: Invalid value NaN (not a number)")
	}
	finished := make(chan struct{})
	go pprof.Do(context.Background(), pprof.Labels("name", "icebath-monitoring-flush"), func(context.Context) {
		e.mu.Lock()
		for e.unfinished > 0 {
			e.idle.Wait()
		}
		e.mu.Unlock()
		close(finished)
	})
	wait := time.Duration(math.MaxInt64)
	if timeout < float64(math.MaxInt64)/1e9 {
		wait = time.Duration(timeout * 1e9)
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-finished:
	case <-t.C:
	}
}

// Queue depth, dispatched and dropped totals and subscriber count
func (e *MonitoringEmitter) Stats() EmitterStats {
	e.mu.Lock()
	queued := len(e.q)
	e.mu.Unlock()
	e.subMu.Lock()
	subs := len(e.subscribers)
	e.subMu.Unlock()
	return EmitterStats{Queued: queued, Dispatched: int(e.dispatched.Load()), Dropped: int(e.dropped.Load()), Subscribers: subs}
}

// Stops the dispatcher, waiting up to two seconds for it to exit
func (e *MonitoringEmitter) Close() {
	e.stopOnce.Do(func() { close(e.stop) })
	e.startMu.Lock()
	done := e.done
	e.startMu.Unlock()
	if done != nil {
		t := time.NewTimer(2 * time.Second)
		select {
		case <-done:
		case <-t.C:
		}
		t.Stop()
	}
	e.started.Store(false)
}

var (
	emitterMu sync.Mutex
	emitter   *MonitoringEmitter
)

// Process wide emitter, created disabled until an exporter subscribes
func GetEmitter() *MonitoringEmitter {
	emitterMu.Lock()
	defer emitterMu.Unlock()
	if emitter == nil {
		emitter = NewMonitoringEmitter(false)
	}
	return emitter
}

// Emits through the process wide emitter
func Emit(event any) { GetEmitter().Emit(event) }

// Swaps the process wide emitter, closing the previous one
func ResetEmitterForTests(em *MonitoringEmitter) {
	emitterMu.Lock()
	defer emitterMu.Unlock()
	if emitter != nil && em != emitter {
		func() {
			defer func() { recover() }()
			emitter.Close()
		}()
	}
	emitter = em
}
