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
	// Share of the way off the right edge, one out of sight
	x    anim.Value
	gone bool
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
	t := &toast{text: text, level: level, until: m.now().Add(ttl), x: anim.NewValueWith(1, anim.Smooth)}
	t.x.Set(0)
	if m.motion == 0 {
		t.x.Snap()
	}
	m.toasts = append(m.toasts, t)
	for extra := len(m.liveToasts()) - maxToasts; extra > 0; extra-- {
		m.dismissToast(m.liveToasts()[0])
	}
	return tea.Batch(m.wake(), tea.Tick(ttl, func(time.Time) tea.Msg { return toastTickMsg{} }))
}

// Calls fn with every toast as it is shown
func (m *Model) OnToast(fn func(text string, level Level)) { m.onToast = fn }

// Text of every toast on screen, oldest first
func (m *Model) Toasts() []string {
	live := m.liveToasts()
	out := make([]string, 0, len(live))
	for _, t := range live {
		out = append(out, t.text)
	}
	return out
}

// Toasts not yet on their way out, oldest first
func (m *Model) liveToasts() []*toast {
	out := make([]*toast, 0, len(m.toasts))
	for _, t := range m.toasts {
		if !t.gone {
			out = append(out, t)
		}
	}
	return out
}

// Sends a toast back off the right edge, dropping it at once when motion is off
func (m *Model) dismissToast(t *toast) {
	t.gone = true
	t.x.Use(anim.Snappy)
	t.x.Set(1)
	if m.motion == 0 {
		t.x.Snap()
		m.dropToasts()
	}
}

func (m *Model) pruneToasts() {
	now := m.now()
	for _, t := range m.liveToasts() {
		if !now.Before(t.until) {
			m.dismissToast(t)
		}
	}
}

// Forgets the toasts that have slid out of sight
func (m *Model) dropToasts() {
	keep := m.toasts[:0]
	for _, t := range m.toasts {
		if !t.gone || !t.x.Settled() {
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
	m.dropToasts()
	return moving
}
