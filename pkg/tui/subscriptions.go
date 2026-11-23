package tui

import (
	tea "charm.land/bubbletea/v2"
)

// Subscription a session card holds on a session of the background service
type subscription struct {
	id     string
	holder *sessionLifecycle
}

// Subscriptions this window holds on the sessions of the background service, one registry for every session card since they live on the one
// connection: which card shows a session under which subscription, and the detach in flight for a session with the steps waiting for it
type subscriptions struct {
	held    map[string]subscription
	flights map[string]*detachFlight
}

func newSubscriptions() *subscriptions {
	return &subscriptions{held: map[string]subscription{}, flights: map[string]*detachFlight{}}
}

// Subscription held on a session, empty when none is
func (r *subscriptions) of(sid string) string { return r.held[sid].id }

// Records the subscription a card attached a session under
func (r *subscriptions) hold(sid, sub string, holder *sessionLifecycle) {
	r.held[sid] = subscription{id: sub, holder: holder}
}

// Forgets a session's subscription once it was detached, unless a newer one took its place
func (r *subscriptions) release(sid, sub string) {
	if r.held[sid].id == sub {
		delete(r.held, sid)
	}
}

// Sessions a card holds a subscription on, with the subscription of each
func (r *subscriptions) heldBy(holder *sessionLifecycle) map[string]string {
	out := map[string]string{}
	for sid, s := range r.held {
		if s.holder == holder {
			out[sid] = s.id
		}
	}
	return out
}

// The connection ended and every subscription with it, so none is detached later; a detach in flight fails on its own and wakes its waiters
func (r *subscriptions) lost() { clear(r.held) }

// Runs fn once the session's detach in flight finished, at once when none is
func (r *subscriptions) afterDetach(sid string, fn func() tea.Cmd) tea.Cmd {
	if flight := r.flights[sid]; flight != nil {
		flight.waiters = append(flight.waiters, fn)
		return nil
	}
	return fn()
}
