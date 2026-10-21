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

// Card that names the exact keys it wants delivered ahead of the deck bindings while focused
type Claimer interface {
	Claims() []string
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

// Card that never folds to a strip, stays in the stack, and is not closed by the close key
type Pinned interface {
	Pinned() bool
}

// Card that declares the smallest content size it is useful at
type Sized interface {
	MinSize() (w, h int)
}

// Card that identifies itself across sessions so a saved layout can find it again
type Keyed interface {
	Key() string
}

// Card whose content may be swept with the mouse to copy text
type Selectable interface {
	Selectable() bool
}

// Card that wraps others and can say whether it holds one
type Holder interface {
	Holds(Card) bool
}

// Sent to a card when it gains or loses the deck focus
type FocusMsg struct {
	Focused bool
}

// Identity used to save and restore a card's place in the layout
func keyOf(c Card) string {
	if k, ok := c.(Keyed); ok {
		return k.Key()
	}
	return c.Title()
}

func isPinned(c Card) bool {
	p, ok := c.(Pinned)
	return ok && p.Pinned()
}

func minSize(c Card) (int, int) {
	if s, ok := c.(Sized); ok {
		return s.MinSize()
	}
	return 0, 0
}

func constraintOf(c Card) constraint {
	w, h := minSize(c)
	return constraint{pinned: isPinned(c), minW: w, minH: h}
}
