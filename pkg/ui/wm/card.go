// Tiling window manager for terminal cards with workspaces, floating dialogs, a command palette and spring motion
package wm

import (
	"encoding/json"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

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

// Card whose text scrolls under a sweep held past its edge, so one sweep can cover more than a window of it
type SweepScroller interface {
	// Window rows the scrolling text fills, the first and one past the last
	SweepArea() (top, bottom int)
	// Scrolls the text by rows, negative toward its start, returning the rows it moved
	SweepScroll(rows int) int
	// Plain text of window rows counted from the first row of the window, rows outside the window included
	SweepRows(from, to int) []string
}

// Card with controls a press takes hold of, such as a scrollbar, the drag then following the pointer instead of sweeping text
type Grabber interface {
	Grabs(x, y int) bool
}

func grabs(c Card, x, y int) bool {
	g, ok := c.(Grabber)
	return ok && g.Grabs(x, y)
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

// Card that paints its content into cells itself, the buffer sized to its content and cleared beforehand
type Painter interface {
	Paint(buf *uv.Buffer)
}

// Raw card that still lets some keys fire the actions of other contexts, such as the window keys of a terminal
type Passer interface {
	Passes(tea.KeyPressMsg) bool
}

func passes(c Card, msg tea.KeyPressMsg) bool {
	p, ok := c.(Passer)
	return ok && p.Passes(msg)
}

// Card handed every pointer motion over its content, in content coordinates
type Tracker interface {
	Tracks() bool
}

func tracks(c Card) bool {
	t, ok := c.(Tracker)
	return ok && t.Tracks()
}

// Message meant for one card alone, delivered to it instead of every card, even once it has closed
type Addressed interface {
	Addressee() Card
}

// Card that moves on its own, given scaled seconds or zero to jump to rest, answering whether its view changed
type Animator interface {
	Animate(dt float64) bool
}

// Card that needs the frame clock running even while the screen rests, such as one refreshing on an interval
type Waker interface {
	Awake() bool
}

func wakes(c Card) bool {
	w, ok := c.(Waker)
	return ok && w.Awake()
}

// Card told the screen cell its content starts at before it paints, for images placed by screen position
type Positioned interface {
	Origin(x, y int)
}

// Card with boxes of content cells that must keep no background, so an image the terminal placed under them shows
type Transparent interface {
	Holes() []Box
}

func holesOf(c Card) []Box {
	if t, ok := c.(Transparent); ok {
		return t.Holes()
	}
	return nil
}

// Card that keeps state between runs, handed back on the next start what it reported
type Stateful interface {
	State() any
	SetState(json.RawMessage) error
}

// Sent to a card when it gains or loses the input focus
type FocusMsg struct {
	Focused bool
}

// Sent to a card once it is closed, before its frame has left the stage
type ClosedMsg struct{}

// Identity used to find a card again, its key or else its title
func keyOf(c Card) string {
	if k, ok := c.(Keyed); ok {
		return k.Key()
	}
	return c.Title()
}

// Glyph a card names for itself, the theme card mark when it names none
func glyphOf(c Card, t theme.Theme) string {
	if g, ok := c.(Glypher); ok {
		return g.Glyph()
	}
	return t.Glyphs.Card
}

// Overlay with a step of its own behind the close key: Back takes the key for that step and reports true, false letting the overlay close
type Backer interface {
	Back() bool
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

// A button on the bottom edge of the focused card. Enter presses the first one, tab moves to the next, a click presses any
type Hint struct {
	Action string
	// Word on the button, the label of the action when empty
	Label string
}

// Card with buttons: the things to do with what it shows now, the one enter does first
type Hinter interface {
	Hints() []Hint
}

// Card that names its kind, such as Chat or Terminal, for the commands the palette lists for it
type Kinded interface {
	Kind() string
}

// Card that says which actions of its contexts it has a use for, so the palette lists those and no others
type Answerer interface {
	Answers(action string) bool
}

func answers(c Card, action string) bool {
	a, ok := c.(Answerer)
	return !ok || a.Answers(action)
}

func hintsOf(c Card) []Hint {
	if h, ok := c.(Hinter); ok {
		return h.Hints()
	}
	return nil
}

// Card that paints a line of its own into its bottom edge, its status, given the columns the two parts may take together
type Footer interface {
	Footer(room int) FooterLine
}

// Status a card paints into its bottom edge, a part at the left end and one at the right end with the border running between them
type FooterLine struct {
	Left, Right string
	// Spans of either part that fire an action when clicked
	Spans []FooterSpan
}

// Columns of a footer part, counted from its first cell, that fire an action when clicked
type FooterSpan struct {
	X0, X1 int
	Right  bool
	Action string
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

// Keymap action fired at the focused card
type ActionMsg struct {
	ID string
}
