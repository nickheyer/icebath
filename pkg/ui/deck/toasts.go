package deck

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/anim"
)

// Most toasts shown at once, older ones dropped first
const maxToasts = 5

type toastTickMsg struct{}

// Transient line shown at the top right corner until it expires
type toast struct {
	text  string
	level Level
	until time.Time
	x     anim.Value
}

// Shows a toast for ttl, the default life when ttl is zero
func (m *Model) Toast(text string, level Level, ttl time.Duration) tea.Cmd {
	if ttl <= 0 {
		ttl = DefaultToastTTL
	}
	t := &toast{text: text, level: level, until: m.now().Add(ttl), x: anim.NewValue(1)}
	t.x.Set(0)
	if m.motion == 0 {
		t.x.Snap()
	}
	m.toasts = append(m.toasts, t)
	if len(m.toasts) > maxToasts {
		m.toasts = m.toasts[len(m.toasts)-maxToasts:]
	}
	return tea.Batch(m.wake(), tea.Tick(ttl, func(time.Time) tea.Msg { return toastTickMsg{} }))
}

// Toast texts on screen, oldest first
func (m *Model) Toasts() []string {
	out := make([]string, 0, len(m.toasts))
	for _, t := range m.toasts {
		out = append(out, t.text)
	}
	return out
}

// Drops every toast past its expiry
func (m *Model) pruneToasts() {
	now := m.now()
	keep := m.toasts[:0]
	for _, t := range m.toasts {
		if now.Before(t.until) {
			keep = append(keep, t)
		}
	}
	clear(m.toasts[len(keep):])
	m.toasts = keep
}

func (m *Model) stepToasts(dt float64) bool {
	m.pruneToasts()
	moving := false
	for _, t := range m.toasts {
		if m.motion == 0 {
			t.x.Snap()
			continue
		}
		moving = t.x.Step(dt/m.motion) || moving
	}
	return moving
}
