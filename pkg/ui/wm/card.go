// Tiling window manager for terminal cards with workspaces, floating dialogs, a command palette and spring motion
package wm

import tea "charm.land/bubbletea/v2"

// Panel the manager places, frames and feeds messages
type Card interface {
	Title() string
	Init() tea.Cmd
	Update(tea.Msg) (Card, tea.Cmd)
	View(width, height int) string
}

// Card with a stable identity, so opening it twice focuses the one already open
type Keyed interface {
	Key() string
}

// Card with an icon shown before its title
type Glypher interface {
	Glyph() string
}

// Card with a short trailer for its bottom edge
type Statuser interface {
	Status() string
}

// Card that places the terminal cursor relative to its content origin
type Cursorer interface {
	Cursor() *tea.Cursor
}

// Card that declares the content size it opens at when floating and the least it is useful at when tiled
type Sized interface {
	MinSize() (w, h int)
}

// Card whose content may be swept with the mouse to copy text
type Selectable interface {
	Selectable() bool
}

// Card that opens as a floating dialog over the tiles
type Floater interface {
	Floating() bool
}

// Card the close command leaves alone when it answers false
type Closable interface {
	Closable() bool
}

// Card that takes every key as typed while it answers true, only the actions of its own contexts firing
type Raw interface {
	Raw() bool
}

func raw(c Card) bool {
	r, ok := c.(Raw)
	return ok && r.Raw()
}

// Card that wraps others and can say whether it holds one
type Holder interface {
	Holds(Card) bool
}

// Sent to a card when it gains or loses the input focus
type FocusMsg struct {
	Focused bool
}

// Identity used to find a card again, its key or else its title
func keyOf(c Card) string {
	if k, ok := c.(Keyed); ok {
		return k.Key()
	}
	return c.Title()
}

func glyphOf(c Card) string {
	if g, ok := c.(Glypher); ok {
		return g.Glyph()
	}
	return "▪"
}

func closable(c Card) bool {
	cl, ok := c.(Closable)
	return !ok || cl.Closable()
}

func floats(c Card) bool {
	f, ok := c.(Floater)
	return ok && f.Floating()
}

func minSize(c Card) (int, int) {
	if s, ok := c.(Sized); ok {
		return s.MinSize()
	}
	return 0, 0
}

// One action a card answers, painted in the bottom edge of the focused card with its bound key
type Hint struct {
	Action string
	// Second action shown with the first as a pair such as j/k
	Pair string
	// Word shown instead of the action label when set
	Label string
}

// Card that names the actions it answers while focused
type Hinter interface {
	Hints() []Hint
}

func hintsOf(c Card) []Hint {
	if h, ok := c.(Hinter); ok {
		return h.Hints()
	}
	return nil
}

// Card that names the keymap contexts in force while it holds the input, most specific first
type Contexter interface {
	Contexts() []string
}

func contextsOf(c Card) []string {
	if k, ok := c.(Contexter); ok {
		return k.Contexts()
	}
	return nil
}

// Keymap action fired at the focused card by the key that was pressed
type ActionMsg struct {
	ID  string
	Key tea.KeyPressMsg
}
