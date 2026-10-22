package wm

import (
	"time"

	tea "charm.land/bubbletea/v2"
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

// Asks the manager to focus the card
func RequestFocus(from Card) tea.Cmd { return intent(Intent{From: from, kind: intentFocus}) }

// Asks the manager to close the card
func RequestClose(from Card) tea.Cmd { return intent(Intent{From: from, kind: intentClose}) }

// Asks the manager to open a card in the workspace of the requesting one
func RequestOpen(from, card Card) tea.Cmd {
	return intent(Intent{From: from, kind: intentOpen, card: card})
}

// Asks the manager to float or tile the card
func RequestFloat(from Card, on bool) tea.Cmd {
	return intent(Intent{From: from, kind: intentFloat, on: on})
}

// Asks the manager to fill the stage with the card or give the stage back
func RequestZoom(from Card, on bool) tea.Cmd {
	return intent(Intent{From: from, kind: intentZoom, on: on})
}

// Asks the manager to glow the card with the urgency until it is focused, Calm clearing it
func RequestAttention(from Card, u Urgency) tea.Cmd {
	return intent(Intent{From: from, kind: intentAttention, urgency: u})
}

// Asks the manager to show a card above everything with first claim on input
func RequestOverlay(card Card) tea.Cmd {
	return intent(Intent{kind: intentOverlay, card: card})
}

// Asks the manager to drop the overlay
func RequestDismiss() tea.Cmd { return intent(Intent{kind: intentDismiss}) }

// Asks the manager to drop the overlay while it is still the card asking
func RequestDismissOf(from Card) tea.Cmd { return intent(Intent{From: from, kind: intentDismiss}) }

// Asks the manager to show a toast for the default life
func Notify(text string, level Level) tea.Cmd {
	return intent(Intent{kind: intentToast, text: text, level: level, ttl: DefaultToastTTL})
}

// Asks the manager to show a toast that lives for ttl
func NotifyFor(text string, level Level, ttl time.Duration) tea.Cmd {
	return intent(Intent{kind: intentToast, text: text, level: level, ttl: ttl})
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
		return m.Open(e.ws, in.card)
	case intentFloat:
		return m.setFloat(e, in.on)
	case intentZoom:
		return m.setZoom(e, in.on)
	case intentAttention:
		return m.attention(e, in.urgency)
	}
	return nil
}
