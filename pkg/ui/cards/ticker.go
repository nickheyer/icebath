package cards

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// Self rescheduling timer of one card whose ticks no other card and no abandoned loop acts on
type ticker struct {
	every time.Duration
	gen   int
	armed bool
}

// Tick a ticker scheduled, naming the ticker and the loop it belongs to
type tickMsg struct {
	owner *ticker
	gen   int
}

// Schedules the next tick unless one is already pending
func (k *ticker) start() tea.Cmd {
	if k.armed {
		return nil
	}
	k.gen++
	k.armed = true
	owner, gen := k, k.gen
	return tea.Tick(k.every, func(time.Time) tea.Msg { return tickMsg{owner: owner, gen: gen} })
}

// Abandons the pending tick so the next start begins a fresh loop
func (k *ticker) reset() { k.armed = false }

// Reports whether the message is the pending tick, disarming it so the owner may schedule the next
func (k *ticker) owns(msg tickMsg) bool {
	if msg.owner != k || msg.gen != k.gen || !k.armed {
		return false
	}
	k.armed = false
	return true
}
