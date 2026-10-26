package wm

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/nickheyer/icebath/pkg/ui/anim"
)

// How urgently a card wants attention, driving the color its frame glows with
type Urgency int

const (
	Calm Urgency = iota
	// Something waits on the person, such as an approval
	Pulse
	// Something failed
	Alarm
)

// Severity of a toast
type Level int

const (
	Info Level = iota
	Success
	Warn
	Error
)

// Default life of a toast
const DefaultToastTTL = 4 * time.Second

type intentKind int

const (
	intentFocus intentKind = iota
	intentClose
	intentOpen
	intentFloat
	intentZoom
	intentAttention
	intentOverlay
	intentDismiss
	intentToast
)

// Request a card makes of the manager, delivered as a message it handles on its own
type Intent struct {
	From    Card
	kind    intentKind
	card    Card
	on      bool
	urgency Urgency
	text    string
	level   Level
	ttl     time.Duration
}

func intent(in Intent) tea.Cmd {
	return func() tea.Msg { return in }
}

// Asks the manager to close the card
func RequestClose(from Card) tea.Cmd { return intent(Intent{From: from, kind: intentClose}) }

// Asks the manager to glow the card with the urgency until it is focused, Calm clearing it
func RequestAttention(from Card, u Urgency) tea.Cmd {
	return intent(Intent{From: from, kind: intentAttention, urgency: u})
}

// Asks the manager to show a card above everything with first claim on input
func RequestOverlay(card Card) tea.Cmd {
	return intent(Intent{kind: intentOverlay, card: card})
}

// Asks the manager to drop the overlay while it is still the card asking
func RequestDismissOf(from Card) tea.Cmd { return intent(Intent{From: from, kind: intentDismiss}) }

// Asks the manager to show a toast for the default life
func Notify(text string, level Level) tea.Cmd {
	return intent(Intent{kind: intentToast, text: text, level: level, ttl: DefaultToastTTL})
}

// Carries out an intent a card returned
func (m *Model) handleIntent(in Intent) tea.Cmd {
	switch in.kind {
	case intentToast:
		return m.Toast(in.text, in.level, in.ttl)
	case intentDismiss:
		if in.From != nil && (m.overlay == nil || m.overlay.card != in.From) {
			return nil
		}
		return m.Dismiss()
	case intentOverlay:
		if in.card == nil {
			return nil
		}
		return m.ShowOverlay(in.card)
	}
	e := m.entryOf(in.From)
	if e == nil {
		return nil
	}
	switch in.kind {
	case intentFocus:
		return m.focusEntry(e)
	case intentClose:
		return m.closeEntry(e)
	case intentOpen:
		if in.card == nil {
			return nil
		}
		return m.OpenFrom(e.ws, in.card, e.frame())
	case intentFloat:
		return m.setFloat(e, in.on)
	case intentZoom:
		return m.setZoom(e, in.on)
	case intentAttention:
		return m.attention(e, in.urgency)
	}
	return nil
}

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
