// Animated card layout with a lit main slot beside a stack that tiles when roomy and folds to title strips
package deck

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Panel that the deck positions, frames, and feeds messages
type Card interface {
	Title() string
	Glyph() string
	Init() tea.Cmd
	Update(tea.Msg) (Card, tea.Cmd)
	View(width, height int) string
}

// Card that swallows keys while focused so deck shortcuts step aside
type Capturer interface {
	Capturing() bool
}

// Card that supplies a short trailer for its bottom edge
type Statuser interface {
	Status() string
}

// Card that places the terminal cursor relative to its content origin
type Cursorer interface {
	Cursor() *tea.Cursor
}

// Card that names the keys it handles while focused so the hint bar can show them
type Hinter interface {
	Hints() []key.Binding
}

// Sent to a card when it gains or loses the deck focus
type FocusMsg struct {
	Focused bool
}
