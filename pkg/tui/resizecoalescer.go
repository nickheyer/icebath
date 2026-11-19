package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// Leading and trailing throttle of terminal resizes, at most one reflow per interval and always the last
type resizeCoalescer struct {
	interval time.Duration
	last     time.Time
	ran      bool
	gen      int
	pending  bool
}

// Trailing reflow of one schedule generation
type resizeReflowMsg struct{ gen int }

func newResizeCoalescer(interval time.Duration) *resizeCoalescer {
	return &resizeCoalescer{interval: interval}
}

// Reflows at once when the interval passed, otherwise arms the trailing reflow for the rest of it
func (r *resizeCoalescer) schedule() (bool, tea.Cmd) {
	r.gen++
	r.pending = false
	elapsed := time.Since(r.last)
	if !r.ran || elapsed >= r.interval {
		r.ran, r.last = true, time.Now()
		return true, nil
	}
	r.pending = true
	gen := r.gen
	return false, tea.Tick(r.interval-elapsed, func(time.Time) tea.Msg { return resizeReflowMsg{gen: gen} })
}

// Whether a trailing reflow is still the armed one, recording it as the last reflow
func (r *resizeCoalescer) fire(m resizeReflowMsg) bool {
	if m.gen != r.gen || !r.pending {
		return false
	}
	r.pending, r.ran, r.last = false, true, time.Now()
	return true
}
