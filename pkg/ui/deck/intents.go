package deck

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
	intentPromote
	intentSibling
	intentClose
	intentAttention
	intentOverlay
	intentDismiss
	intentToast
	intentMode
)

// Request a card makes of the deck, delivered as a message the deck handles on its own
type Intent struct {
	From    Card
	kind    intentKind
	card    Card
	urgency Urgency
	text    string
	level   Level
	ttl     time.Duration
	mode    Mode
}

func intent(in Intent) tea.Cmd {
	return func() tea.Msg { return in }
}

// Asks the deck to focus the card
func RequestFocus(from Card) tea.Cmd { return intent(Intent{From: from, kind: intentFocus}) }

// Asks the deck to trade the card into the main slot
func RequestPromote(from Card) tea.Cmd { return intent(Intent{From: from, kind: intentPromote}) }

// Asks the deck to deal a new card next to the requesting one
func RequestSibling(from, sibling Card) tea.Cmd {
	return intent(Intent{From: from, kind: intentSibling, card: sibling})
}

// Asks the deck to retire the card
func RequestClose(from Card) tea.Cmd { return intent(Intent{From: from, kind: intentClose}) }

// Asks the deck to glow the card with the urgency until it is focused, Calm clearing it
func RequestAttention(from Card, u Urgency) tea.Cmd {
	return intent(Intent{From: from, kind: intentAttention, urgency: u})
}

// Asks the deck to show a card above everything, with first claim on input
func RequestOverlay(from, overlay Card) tea.Cmd {
	return intent(Intent{From: from, kind: intentOverlay, card: overlay})
}

// Asks the deck to drop the overlay
func RequestDismiss(from Card) tea.Cmd { return intent(Intent{From: from, kind: intentDismiss}) }

// Asks the deck to show a toast for the default life
func Notify(text string, level Level) tea.Cmd {
	return intent(Intent{kind: intentToast, text: text, level: level, ttl: DefaultToastTTL})
}

// Asks the deck to show a toast that lives for ttl
func NotifyFor(text string, level Level, ttl time.Duration) tea.Cmd {
	return intent(Intent{kind: intentToast, text: text, level: level, ttl: ttl})
}

// Asks the deck to switch layout mode
func RequestMode(from Card, mode Mode) tea.Cmd {
	return intent(Intent{From: from, kind: intentMode, mode: mode})
}

// Index of the entry holding the card, directly or through a holder, or -1
func (m *Model) indexOf(c Card) int {
	if c == nil {
		return -1
	}
	for i, e := range m.cards {
		if e.card == c {
			return i
		}
		if h, ok := e.card.(Holder); ok && h.Holds(c) {
			return i
		}
	}
	return -1
}

// Carries out an intent a card returned
func (m *Model) handleIntent(in Intent) tea.Cmd {
	switch in.kind {
	case intentToast:
		return m.Toast(in.text, in.level, in.ttl)
	case intentDismiss:
		return m.Dismiss()
	case intentMode:
		return m.SetMode(in.mode)
	case intentOverlay:
		if in.card == nil {
			return nil
		}
		return m.ShowOverlay(in.card)
	}
	i := m.indexOf(in.From)
	if i < 0 {
		return nil
	}
	switch in.kind {
	case intentFocus:
		return m.Focus(i)
	case intentPromote:
		return m.Promote(i)
	case intentSibling:
		if in.card == nil {
			return nil
		}
		return m.AddAfter(i, in.card)
	case intentClose:
		return m.Remove(i)
	case intentAttention:
		return m.Attention(i, in.urgency)
	}
	return nil
}
