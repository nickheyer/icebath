package hooks

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	hooksconfig "github.com/nickheyer/icebath/internal/hooks/config"
)

// Events one stream observer may have waiting, the oldest dropped past it
const streamQueue = 1024

// Workers of the stream observers, one for each callback of a stream point, so a slow observer never holds the reply up
type streams struct {
	mu      sync.Mutex
	workers map[slot]*worker
}

type worker struct {
	events chan queued
	done   chan struct{}
}

// One event waiting for an observer, under the context of the turn that streamed it
type queued struct {
	ctx   context.Context
	hook  Hook
	event Event
	stop  bool
}

// Queues the event for every callback of its point, each run on a worker of its own in the order the events came; whether one took it
func (h *Hooks) Stream(ctx context.Context, cfg *config.Config, event Event) bool {
	if h == nil {
		return false
	}
	hooks := h.set(cfg, event.Point())
	live := map[slot]bool{}
	for _, hook := range hooks {
		live[slot{point: event.Point(), name: hook.Name}] = true
	}
	h.streams.mu.Lock()
	var stale []*worker
	for key, w := range h.streams.workers {
		if key.point == event.Point() && !live[key] {
			stale = append(stale, w)
			delete(h.streams.workers, key)
		}
	}
	taken := false
	for _, hook := range hooks {
		key := slot{point: event.Point(), name: hook.Name}
		w := h.streams.workers[key]
		if w == nil || w.exited() {
			w = &worker{events: make(chan queued, streamQueue), done: make(chan struct{})}
			h.streams.workers[key] = w
			go h.observe(key, w)
		}
		if w.put(queued{ctx: context.WithoutCancel(ctx), hook: hook, event: event}) {
			taken = true
		} else {
			slog.Debug("plugin stream hook queue full after drop-oldest: " + event.Point() + " callback=" + hook.Name)
		}
	}
	h.streams.mu.Unlock()
	for _, w := range stale {
		w.put(queued{stop: true})
		select {
		case <-w.done:
		case <-time.After(200 * time.Millisecond):
		}
	}
	return taken
}

// Whether a reasoning piece of a reply is one for the hooks: hooks.stream_reasoning lets them see it and a callback watches the pieces
func (h *Hooks) StreamsReasoning(cfg *config.Config) bool {
	return cfg.GetBool(hooksconfig.StreamReasoning) && h.Has(cfg, StreamDelta)
}

// Runs the events of one observer until it is stopped, each failure logged once
func (h *Hooks) observe(key slot, w *worker) {
	defer close(w.done)
	for item := range w.events {
		if item.stop {
			return
		}
		if _, err := run(item.ctx, item.hook, item.event); err != nil {
			h.report(key.point, key.name, item.event, err)
		}
	}
}

func (w *worker) exited() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// Queues the item, the oldest waiting dropped once when the queue is full
func (w *worker) put(item queued) bool {
	select {
	case w.events <- item:
		return true
	default:
	}
	select {
	case <-w.events:
	default:
	}
	select {
	case w.events <- item:
		return true
	default:
		return false
	}
}
