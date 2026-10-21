// Terminal application that composes the harness, the deck and the cards, with a headless runner for scripts
package app

import (
	"sync"

	tea "charm.land/bubbletea/v2"
)

// Sender that queues messages until a program exists to receive them
type relay struct {
	mu      sync.Mutex
	program *tea.Program
	queue   []tea.Msg
	closed  bool
}

func (r *relay) Send(msg any) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if r.program == nil {
		r.queue = append(r.queue, msg)
		r.mu.Unlock()
		return
	}
	p := r.program
	r.mu.Unlock()
	p.Send(msg)
}

// Attaches the program and flushes everything queued before it existed
func (r *relay) Attach(p *tea.Program) {
	r.mu.Lock()
	r.program = p
	queued := r.queue
	r.queue = nil
	r.mu.Unlock()
	for _, m := range queued {
		p.Send(m)
	}
}

// Drops every later message, for shutdown
func (r *relay) Close() {
	r.mu.Lock()
	r.closed = true
	r.program = nil
	r.mu.Unlock()
}
