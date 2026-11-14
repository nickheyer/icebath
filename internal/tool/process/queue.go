package process

import (
	"context"
	"log/slog"
	"time"

	"github.com/nickheyer/icebath/internal/tool"
)

type Notification struct {
	Event Event
	Text  string
}

func (n Notification) Notice() tool.Notice {
	kind := n.Event.Type
	if kind == "" {
		kind = EventCompletion
	}
	return tool.Notice{
		Kind:       kind,
		SessionKey: n.Event.SessionKey,
		TaskID:     n.Event.spawner(),
		ProcessID:  n.Event.SessionID,
		Text:       n.Text,
		Route:      tool.Target{Platform: n.Event.Route.Platform, ChatID: n.Event.Route.ChatID, ThreadID: n.Event.Route.ThreadID},
	}
}

type DrainOptions struct {
	SessionKey       string
	Owns             func(Event) bool
	KeepPollObserved bool
}

func (r *Registry) Put(ev Event) { r.events.put(ev) }

func (r *Registry) Inspect(fn func(queued []Event)) {
	r.events.mu.Lock()
	defer r.events.mu.Unlock()
	fn(append([]Event(nil), r.events.items...))
}

func (r *Registry) Pending() int {
	r.events.mu.Lock()
	defer r.events.mu.Unlock()
	return len(r.events.items)
}

func (r *Registry) adjustInFlightLocked(events []Event, delta int) {
	for _, ev := range events {
		o := ev.owner()
		if o == "" {
			continue
		}
		r.events.inFlight[o] += delta
		if r.events.inFlight[o] <= 0 {
			delete(r.events.inFlight, o)
		}
	}
}

func (r *Registry) TakeReady(ctx context.Context, timeout time.Duration) []Event {
	q := &r.events
	deadline := time.Now().Add(timeout)
	q.mu.Lock()
	for len(q.items) == 0 && timeout > 0 {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		q.mu.Unlock()
		timer := time.NewTimer(remaining)
		select {
		case <-q.ready:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		q.mu.Lock()
		if ctx.Err() != nil {
			break
		}
	}
	events := q.items
	q.items = nil
	if len(events) > 0 {
		r.adjustInFlightLocked(events, 1)
		for _, ev := range events {
			if id := ev.CopyID(); id != "" {
				r.delegation.Hold(id)
			}
		}
	}
	q.mu.Unlock()
	return events
}

func (r *Registry) Release(events []Event) {
	if len(events) == 0 {
		return
	}
	r.events.mu.Lock()
	r.adjustInFlightLocked(events, -1)
	r.events.mu.Unlock()
	for _, ev := range events {
		if id := ev.CopyID(); id != "" {
			r.delegation.Unhold(id)
		}
	}
}

func (r *Registry) DrainedFor(ctx context.Context, owns func(Event) bool) ([]Event, func()) {
	r.RestoreCompletions(ctx)
	ready := r.TakeReady(ctx, 0)
	var mine []Event
	for _, ev := range ready {
		if owns(ev) {
			mine = append(mine, ev)
		} else {
			r.events.put(ev)
		}
	}
	return mine, func() { r.Release(ready) }
}

func (r *Registry) RestoreCompletions(ctx context.Context) int {
	restored := 0
	r.restoreOnce.Do(func() {
		n, err := r.delegation.Restore(context.WithoutCancel(ctx), r.events.put)
		if err != nil {
			slog.Warn("Could not restore async delegation completions", "err", err)
			return
		}
		restored = n
	})
	return restored
}

func OwnsEvent(ev Event, sessionKey string, owns func(Event) bool) bool {
	delegation := ev.Type == EventAsyncDelegation
	proof := delegation || ev.SessionKey != "" || ev.OriginUISessionID != ""
	if owns != nil && proof {
		return owns(ev)
	}
	if sessionKey != "" && proof {
		return ev.SessionKey == sessionKey
	}
	return !(delegation && ev.Restored)
}

func (r *Registry) drainShouldSkip(id string, skipPollObserved bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.consumed[id] || skipPollObserved && r.pollObserved[id]
}

func (r *Registry) surfaceChild(ctx context.Context) bool {
	c, err := r.cfg.LoadReadonly(ctx)
	if err != nil {
		return false
	}
	return c.Delegation.SurfaceChildProcessNotifications
}

func (r *Registry) ShouldSurface(ctx context.Context, ev Event) bool {
	if !IsChildNotification(ev) {
		return true
	}
	return r.surfaceChild(ctx)
}

func (r *Registry) Drain(ctx context.Context, o DrainOptions) []Notification {
	r.RestoreCompletions(ctx)
	r.events.mu.Lock()
	events := r.events.items
	r.events.items = nil
	r.events.mu.Unlock()
	var out []Notification
	var requeue []Event
	surface, surfaceRead := false, false
	for _, ev := range events {
		if !OwnsEvent(ev, o.SessionKey, o.Owns) {
			requeue = append(requeue, ev)
			continue
		}
		if (ev.Type == "" || ev.Type == EventCompletion) && r.drainShouldSkip(ev.SessionID, !o.KeepPollObserved) {
			continue
		}
		if IsChildNotification(ev) {
			if !surfaceRead {
				surface, surfaceRead = r.surfaceChild(ctx), true
			}
			if !surface {
				kind := ev.Type
				if kind == "" {
					kind = EventCompletion
				}
				slog.Debug("Suppressed subagent-owned process notification "+
					"(delegation.surface_child_process_notifications=false)",
					"type", kind, "session_id", ev.SessionID, "task_id", ev.spawner())
				continue
			}
		}
		if text := r.Render(ctx, ev); text != "" {
			out = append(out, Notification{Event: ev, Text: text})
		}
	}
	for _, ev := range requeue {
		r.events.put(ev)
	}
	return out
}

func notifies(s *Session) bool {
	return s.notify || len(s.watchPatterns) > 0 && !s.watchDisabled
}

func (r *Registry) anyRunning(ctx context.Context, pred func(s *Session) bool) bool {
	r.mu.Lock()
	live := r.running.list()
	r.mu.Unlock()
	for _, s := range live {
		r.refreshDetached(ctx, s)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.running.list() {
		s.mu.Lock()
		hit := !s.exited && pred(s)
		s.mu.Unlock()
		if hit {
			return true
		}
	}
	return false
}

func (r *Registry) AwaitsNotification(ctx context.Context, ownerTaskIDs ...string) bool {
	owners := map[string]bool{}
	for _, o := range ownerTaskIDs {
		if o != "" {
			owners[o] = true
		}
	}
	if len(owners) == 0 {
		return false
	}
	if r.anyRunning(ctx, func(s *Session) bool { return owners[s.ownerTaskID] && notifies(s) }) {
		return true
	}
	r.mu.Lock()
	for _, s := range append(r.running.list(), r.finished.list()...) {
		s.mu.Lock()
		pending := owners[s.ownerTaskID] && s.notify && s.exited
		s.mu.Unlock()
		if pending && !s.isDone() {
			r.mu.Unlock()
			return true
		}
	}
	r.mu.Unlock()
	q := &r.events
	q.mu.Lock()
	defer q.mu.Unlock()
	for o := range owners {
		if q.inFlight[o] > 0 {
			return true
		}
	}
	for _, ev := range q.items {
		if owners[ev.owner()] {
			return true
		}
	}
	return false
}
