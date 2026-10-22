package wm

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/anim"
)

// Short notice that slides in at the top right and leaves on its own
type toast struct {
	text  string
	level Level
	until time.Time
	x     anim.Value
}

type toastTickMsg struct{}

// Shows a toast for ttl, the default life when ttl is zero
func (m *Model) Toast(text string, level Level, ttl time.Duration) tea.Cmd {
	if text == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = DefaultToastTTL
	}
	if m.onToast != nil {
		m.onToast(text, level)
	}
	t := &toast{text: text, level: level, until: m.now().Add(ttl), x: fast(1)}
	t.x.Set(0)
	if m.motion == 0 {
		t.x.Snap()
	}
	m.toasts = append(m.toasts, t)
	if len(m.toasts) > 5 {
		m.toasts = m.toasts[len(m.toasts)-5:]
	}
	return tea.Batch(m.wake(), tea.Tick(ttl, func(time.Time) tea.Msg { return toastTickMsg{} }))
}

// Calls fn with every toast as it is shown
func (m *Model) OnToast(fn func(text string, level Level)) { m.onToast = fn }

// Text of every toast on screen, oldest first
func (m *Model) Toasts() []string {
	out := make([]string, 0, len(m.toasts))
	for _, t := range m.toasts {
		out = append(out, t.text)
	}
	return out
}

func (m *Model) pruneToasts() {
	now := m.now()
	keep := m.toasts[:0]
	for _, t := range m.toasts {
		if now.Before(t.until) {
			keep = append(keep, t)
		}
	}
	m.toasts = keep
}

func (m *Model) stepToasts(dt float64) bool {
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
