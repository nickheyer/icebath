package logging

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Unbounded FIFO of records, nil standing for the listener stop sentinel
type simpleQueue struct {
	mu    sync.Mutex
	cond  *sync.Cond
	items []*Record
}

func newSimpleQueue() *simpleQueue {
	q := &simpleQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *simpleQueue) put(rec *Record) {
	q.mu.Lock()
	q.items = append(q.items, rec)
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *simpleQueue) get() *Record {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 {
		q.cond.Wait()
	}
	rec := q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	return rec
}

// Root handler that enqueues a copy of each record for the listener to format
type queueHandler struct {
	handlerCore
	queue *simpleQueue
}

func (h *queueHandler) Handle(_ context.Context, rec *Record) bool {
	if !h.filter(rec) {
		return false
	}
	h.lock.Lock()
	defer h.lock.Unlock()
	cp := *rec
	h.queue.put(&cp)
	return true
}

func (h *queueHandler) Close() error {
	unregisterHandler(h)
	return nil
}

// Goroutine draining the queue into its handler set, each handler applying its own level
type queueListener struct {
	queue    *simpleQueue
	handlers []Handler
	mu       sync.Mutex
	done     chan struct{}
}

// Starts the worker goroutine unless one is running
func (l *queueListener) start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done != nil {
		return
	}
	done := make(chan struct{})
	l.done = done
	go func() {
		defer close(done)
		for {
			rec := l.queue.get()
			if rec == nil {
				return
			}
			for _, h := range l.handlers {
				if rec.Levelno >= h.Level() {
					h.Handle(context.Background(), rec)
				}
			}
		}
	}()
}

// Enqueues the sentinel and waits until the worker wrote every record queued before it
func (l *queueListener) stop() error {
	l.mu.Lock()
	done := l.done
	l.done = nil
	l.mu.Unlock()
	if done == nil {
		return nil
	}
	l.queue.put(nil)
	<-done
	return nil
}

var queueState struct {
	mu       sync.Mutex
	queue    *simpleQueue
	listener *queueListener
	handlers []Handler
}

// Stops the listener after it writes every pending record
func stopQueueListener() {
	queueState.mu.Lock()
	defer queueState.mu.Unlock()
	l := queueState.listener
	queueState.listener = nil
	if l != nil {
		quietly(l.stop)
	}
}

// Replaces the listener with one over the current handler set, caller holding the queue state lock
func startQueueListenerLocked() {
	if queueState.listener != nil {
		_ = queueState.listener.stop()
	}
	queueState.listener = &queueListener{queue: queueState.queue, handlers: slices.Clone(queueState.handlers)}
	queueState.listener.start()
}

// Drives handler through the shared queue so emitting goroutines never wait on file I/O or the rotation lock
func registerQueuedHandler(h Handler) {
	queueState.mu.Lock()
	defer queueState.mu.Unlock()
	if queueState.queue == nil {
		queueState.queue = newSimpleQueue()
		qh := &queueHandler{queue: queueState.queue}
		registerHandler(qh)
		root.AddHandler(qh)
	}
	queueState.handlers = append(queueState.handlers, h)
	startQueueListenerLocked()
	installSlogDefault()
}

// Blocks until every queued record is written, then resumes the listener
func FlushLogQueue() {
	queueState.mu.Lock()
	defer queueState.mu.Unlock()
	if l := queueState.listener; l != nil {
		_ = l.stop()
		l.start()
	}
}

// Drains the queue for a hard exit, waiting at most timeout for a listener wedged on the rotation lock
func DrainLogQueue(timeout time.Duration) {
	queueState.mu.Lock()
	l := queueState.listener
	queueState.mu.Unlock()
	if l == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		quietly(l.stop)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// Closes this process's routed log files below a profile home before the profile is removed
func ReleaseProfileLogHandlers(profileHome string) (int, error) {
	h, err := resolveHome(profileHome)
	if err != nil {
		return 0, err
	}
	queueState.mu.Lock()
	defer queueState.mu.Unlock()
	l := queueState.listener
	if l != nil {
		_ = l.stop()
		queueState.listener = nil
	}
	released := 0
	for _, handler := range queueState.handlers {
		if r, ok := handler.(*profileRoutingFileHandler); ok && r.releaseProfile(h) {
			released++
		}
	}
	if l != nil {
		startQueueListenerLocked()
	}
	return released, nil
}

// Replaces the queued file handlers with profile routers over homes, true when routing is or already was on
func EnableProfileLogRouting(profileHomes []string) (bool, error) {
	var homes []string
	for _, entry := range profileHomes {
		resolved, err := resolveHome(entry)
		if err != nil {
			continue
		}
		if !slices.Contains(homes, resolved) {
			homes = append(homes, resolved)
		}
	}
	if len(homes) < 2 {
		return false, nil
	}
	if ResolveProfilePolicy == nil {
		return false, errRedactUnlinked
	}
	for _, h := range homes {
		ResolveProfilePolicy(h)
	}
	queueState.mu.Lock()
	defer queueState.mu.Unlock()
	if len(queueState.handlers) == 0 {
		return false, nil
	}
	var routers []*profileRoutingFileHandler
	for _, handler := range queueState.handlers {
		if r, ok := handler.(*profileRoutingFileHandler); ok {
			routers = append(routers, r)
		}
	}
	if len(routers) > 0 {
		for _, r := range routers {
			r.mu.Lock()
			for _, h := range homes {
				r.profileHomes[h] = true
			}
			r.mu.Unlock()
		}
		return true, nil
	}
	l := queueState.listener
	if l != nil {
		_ = l.stop()
		queueState.listener = nil
	}
	replacement := make([]Handler, 0, len(queueState.handlers))
	for _, existing := range queueState.handlers {
		if m, ok := existing.(*managedRotatingFileHandler); ok {
			replacement = append(replacement, newProfileRoutingFileHandler(m, homes))
			quietly(m.Close)
		} else {
			replacement = append(replacement, existing)
		}
	}
	queueState.handlers = replacement
	if l != nil {
		startQueueListenerLocked()
	}
	return true, nil
}

// Tears down the queue, the listener and every queued handler
func resetQueuedHandlers() {
	stopQueueListener()
	queueState.mu.Lock()
	defer queueState.mu.Unlock()
	for _, h := range root.Handlers() {
		if qh, ok := h.(*queueHandler); ok {
			root.RemoveHandler(qh)
			quietly(qh.Close)
		}
	}
	for _, h := range queueState.handlers {
		quietly(h.Close)
	}
	queueState.handlers = nil
	queueState.queue = nil
}

// Homes the queued file handlers serve, caller holding the queue state lock
func knownLogHomes() map[string]bool {
	homes := map[string]bool{}
	for _, handler := range queueState.handlers {
		switch h := handler.(type) {
		case *profileRoutingFileHandler:
			homes[h.defaultHome] = true
			for _, p := range h.routedHomes() {
				homes[p] = true
			}
		case *managedRotatingFileHandler:
			homes[filepath.Dir(filepath.Dir(resolvePath(h.baseFilename)))] = true
		}
	}
	return homes
}
