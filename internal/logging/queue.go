package logging

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/home"
)

// Records and bytes queued before records below warning are dropped, the larger reserve warnings and above may still fill
const (
	maxQueued        = 1 << 16
	maxQueuedBytes   = 32 << 20
	maxReserved      = 1 << 17
	maxReservedBytes = 64 << 20
	recordOverhead   = 96
)

// Bounded first in first out buffer between the logging goroutines and the listener
type queue struct {
	mu       sync.Mutex
	ready    *sync.Cond
	items    []entry
	head     int
	bytes    int
	seq      uint64
	dropped  drops
	stopping bool
}

// Records dropped since the listener last reported, in all and at warning or above
type drops struct {
	all    int
	severe int
}

// Queued record, or a control step the listener runs in order with the records around it
type entry struct {
	rec  *record
	op   func()
	done chan struct{}
	stop bool
}

// Empty queue ready for the listener
func newQueue() *queue {
	q := &queue{}
	q.ready = sync.NewCond(&q.mu)
	return q
}

// Appends rec without waiting, counting it dropped when the queue is full for its level, taking none once stopping
func (q *queue) push(rec *record) {
	rec.size = len(rec.message) + len(rec.name) + len(rec.session) + len(rec.home) + recordOverhead
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopping {
		return
	}
	queued := len(q.items) - q.head
	full := queued >= maxQueued || q.bytes+rec.size > maxQueuedBytes
	if rec.level >= slog.LevelWarn {
		full = queued >= maxReserved || q.bytes+rec.size > maxReservedBytes
	}
	if full {
		q.dropped.all++
		if rec.level >= slog.LevelWarn {
			q.dropped.severe++
		}
		return
	}
	rec.seq = q.seq
	q.seq++
	q.bytes += rec.size
	q.items = append(q.items, entry{rec: rec})
	q.ready.Signal()
}

// Appends a listener step past the bounds, false once the listener was told to stop
func (q *queue) control(e entry) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopping {
		return false
	}
	q.stopping = e.stop
	q.items = append(q.items, e)
	q.ready.Signal()
	return true
}

// Sequence number the following queued record receives
func (q *queue) next() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.seq
}

// Oldest entry, waiting for one, with the records dropped since the last call
func (q *queue) pop() (entry, drops) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.head == len(q.items) {
		q.ready.Wait()
	}
	e := q.items[q.head]
	q.items[q.head] = entry{}
	q.head++
	if q.head == len(q.items) {
		q.items, q.head = q.items[:0], 0
	} else if q.head > 1024 && q.head*2 > len(q.items) {
		q.items, q.head = append(q.items[:0:0], q.items[q.head:]...), 0
	}
	if e.rec != nil {
		q.bytes -= e.rec.size
	}
	dropped := q.dropped
	q.dropped = drops{}
	return e, dropped
}

// Writes every queued record to its sinks in order until a stop step, then closes done
func (s *System) listen() {
	defer close(s.done)
	for {
		e, dropped := s.q.pop()
		if dropped.all > 0 {
			s.dispatch(&record{time: time.Now(), level: slog.LevelWarn, name: "logging", seq: ^uint64(0),
				message: fmt.Sprintf("log queue overflowed while the log writer was stalled, %d records were dropped, %d of them at WARNING or above",
					dropped.all, dropped.severe)})
		}
		switch {
		case e.rec != nil:
			s.dispatch(e.rec)
		case e.op != nil:
			e.op()
		}
		if e.done != nil {
			close(e.done)
		}
		if e.stop {
			return
		}
	}
}

// Runs op on the listener after every record queued before it, inline once the listener has exited
func (s *System) run(op func()) {
	done := make(chan struct{})
	if s.q.control(entry{op: op, done: done}) {
		<-done
		return
	}
	<-s.done
	s.inline.Lock()
	defer s.inline.Unlock()
	op()
}

// Blocks until every record queued before the call reached its files
func (s *System) Flush() {
	done := make(chan struct{})
	if s.q.control(entry{done: done}) {
		<-done
	}
}

// Writes what is queued and stops the listener for a hard exit, waiting at most timeout for a wedged write
func (s *System) Drain(timeout time.Duration) {
	s.q.control(entry{stop: true})
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-s.done:
	case <-t.C:
	}
}

// Writes every queued record, restores redirected stdio and closes every log file, later records going nowhere
func (s *System) Close() {
	s.closeOnce.Do(func() {
		s.q.control(entry{stop: true})
		<-s.done
		s.restoreStdio()
		s.inline.Lock()
		defer s.inline.Unlock()
		s.mu.Lock()
		sets := s.sets
		dedicated := make([]*dedicated, 0, len(s.dedicated))
		for _, d := range s.dedicated {
			dedicated = append(dedicated, d)
		}
		s.mu.Unlock()
		for _, fs := range sets {
			for _, f := range fs.files {
				f.close()
			}
			clear(fs.files)
		}
		for _, d := range dedicated {
			d.file.close()
		}
	})
}

// Sends one record to its dedicated file or to every shared file set that takes it
func (s *System) dispatch(rec *record) {
	if rec.sink != nil {
		ctx := context.Background()
		if rec.home != "" {
			ctx = home.WithOverride(ctx, rec.home)
		}
		rec.sink.file.append(s.redact.Redact(ctx, rec.sink.layout.line(rec)))
		return
	}
	s.mu.Lock()
	sets := s.sets
	routing := s.routing
	s.mu.Unlock()
	for _, fs := range sets {
		if rec.seq < fs.since || rec.level < fs.level || !fs.takes(rec.name) {
			continue
		}
		s.deliver(fs, rec, routing)
	}
}
