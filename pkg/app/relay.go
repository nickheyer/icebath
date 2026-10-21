// Terminal application that composes the harness, the deck and the cards, with a headless runner for scripts
package app

import (
	"sync"

	tea "charm.land/bubbletea/v2"
)

// Sender whose own goroutine hands queued messages to the program so a send from the event loop never blocks
type relay struct {
	mu      sync.Mutex
	program *tea.Program
	queue   []tea.Msg
	closed  bool
	pumping bool
}

func (r *relay) Send(msg any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.queue = append(r.queue, msg)
	r.drain()
}

// Attaches the program, queued messages reaching it once its event loop runs
func (r *relay) Attach(p *tea.Program) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.program = p
	r.drain()
}

// Drops every queued and later message, for shutdown
func (r *relay) Close() {
	r.mu.Lock()
	r.closed = true
	r.program = nil
	r.queue = nil
	r.mu.Unlock()
}

// Starts the delivery goroutine under the lock when a program is attached and none is running
func (r *relay) drain() {
	if r.program == nil || r.pumping || len(r.queue) == 0 {
		return
	}
	r.pumping = true
	go r.pump()
}

// Hands queued messages to the program in order until the queue empties, the relay closes or the program detaches
func (r *relay) pump() {
	for {
		r.mu.Lock()
		if r.closed || r.program == nil || len(r.queue) == 0 {
			r.pumping = false
			r.mu.Unlock()
			return
		}
		msg, p := r.queue[0], r.program
		r.queue[0] = nil
		r.queue = r.queue[1:]
		r.mu.Unlock()
		p.Send(msg)
	}
}
