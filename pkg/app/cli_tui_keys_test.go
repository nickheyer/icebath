package app

import (
	"sync"
	"time"
)

// burstRig is a prompt on a controlled clock whose timers and loop hops run on demand
type burstRig struct {
	c         *CLI
	app       *ptApplication
	buf       *ptBuffer
	now       float64
	mu        sync.Mutex
	scheduled []func()
	delays    []time.Duration
	busy      []string
}

func (r *burstRig) advance(seconds float64) {
	r.mu.Lock()
	r.now += seconds
	r.mu.Unlock()
}

// Lets the quiet window pass with no input, then fires what was scheduled
func (r *burstRig) runScheduled() {
	pending, delays := r.scheduled, r.delays
	r.scheduled, r.delays = nil, nil
	for i, fn := range pending {
		r.advance(delays[i].Seconds())
		fn()
	}
}

// Everything submitted to the pending input queue so far
func (r *burstRig) submitted() []any {
	var items []any
	for {
		item, ok := r.c.pendingInput.GetNoWait()
		if !ok {
			return items
		}
		items = append(items, item)
	}
}

func (r *burstRig) typeText(text string) { r.buf.insertText(text, false, true, true) }
