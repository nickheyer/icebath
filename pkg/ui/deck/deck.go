package deck

import (
	"image/color"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

const (
	dealStagger  = 0.06
	decodeTime   = 0.3
	flowSpeed    = 16.0
	liftDistance = 2.0
	nudgeImpulse = 26.0
	doubleClick  = 350 * time.Millisecond
	ratioStep    = 0.05
	minRatio     = 0.25
	maxRatio     = 0.8
	fastSpring   = 18.0
)

type entry struct {
	id     int
	card   Card
	rect   anim.Rect
	glow   anim.Value
	hover  anim.Value
	lift   anim.Value
	decode float64
	delay  float64
	dim    float64
	placed bool
	held   bool
	dirty  bool
	raw    screen
	tinted screen
}

// Card picked up by its frame, with the pointer offset inside it and whether it has moved yet
type grab struct {
	e      *entry
	dx, dy int
	x, y   int
	moved  bool
}

// Deck of cards that animates between layouts and routes input to the focused card
type Model struct {
	Keys      KeyMap
	theme     theme.Theme
	clock     anim.Clock
	cards     []*entry
	leaving   []*entry
	boxes     []box
	pending   []tea.Cmd
	zones     []zone
	grab      *grab
	focus     int
	stackMemo int
	drop      int
	hover     int
	hintHot   int
	ratio     float64
	mode      Mode
	width     int
	height    int
	hints     bool
	gutterHot bool
	dragging  bool
	lastClick time.Time
	clickID   int
	phase     float64
	tick      int
	nextID    int
	dealt     int
	title     string
	now       func() time.Time
	rings     map[int][]color.Color
	cells     map[string]uv.Cell
	fade      map[color.Color]color.Color
	buf       screen
}

// Configures a deck at construction
type Option func(*Model)

// Seeds the deck with cards in main then stack order
func WithCards(cards ...Card) Option {
	return func(m *Model) {
		for _, c := range cards {
			m.pending = append(m.pending, m.Add(c))
		}
	}
}

// Replaces the Blackwall palette
func WithTheme(t theme.Theme) Option {
	return func(m *Model) {
		m.theme = t
		m.rings = map[int][]color.Color{}
	}
}

// Sets the share of the width given to the main slot
func WithRatio(r float64) Option {
	return func(m *Model) { m.ratio = clampRatio(r) }
}

// Sets the frame rates used while animating and while at rest
func WithFPS(active, idle int) Option {
	return func(m *Model) { m.clock = anim.NewClock(active, idle) }
}

// Replaces the default bindings
func WithKeyMap(k KeyMap) Option {
	return func(m *Model) { m.Keys = k }
}

// Sets the terminal window title
func WithWindowTitle(title string) Option {
	return func(m *Model) { m.title = title }
}

// Shows or hides the key hint bar and card ordinals at startup
func WithHints(show bool) Option {
	return func(m *Model) { m.hints = show }
}

// Builds a deck with the Blackwall theme, a 62 percent main slot, hints on, and 60 over 20 frame rates
func New(opts ...Option) *Model {
	m := &Model{
		Keys:      DefaultKeyMap(),
		theme:     theme.Blackwall(),
		clock:     anim.NewClock(60, 20),
		ratio:     0.62,
		hover:     -1,
		drop:      -1,
		hintHot:   -1,
		stackMemo: 1,
		hints:     true,
		now:       time.Now,
		rings:     map[int][]color.Color{},
		cells:     map[string]uv.Cell{},
		fade:      map[color.Color]color.Color{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Starts the frame clock and the cards seeded at construction
func (m *Model) Init() tea.Cmd {
	cmds := append(m.pending, m.clock.Wake())
	m.pending = nil
	return tea.Batch(cmds...)
}

// Routes frames to the animation, keys to the focused card, and the mouse to the card under it
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, m.clock.Wake()
	case anim.FrameMsg:
		frame, ok := m.clock.Accept(msg)
		if !ok {
			return m, nil
		}
		return m, m.clock.Next(m.Advance(frame.DT))
	case tea.KeyPressMsg:
		return m, m.handleKey(msg)
	case tea.MouseMsg:
		return m, m.handleMouse(msg)
	}
	return m, m.broadcast(msg)
}

// Deals a new card into the stack from the screen edge
func (m *Model) Add(c Card) tea.Cmd {
	e := &entry{id: m.nextID, card: c, rect: anim.NewRect(0, 0, 0, 0), glow: fast(0), hover: fast(0), lift: fast(0), dim: -1, dirty: true}
	m.nextID++
	m.cards = append(m.cards, e)
	cmds := []tea.Cmd{c.Init()}
	if len(m.cards) == 1 {
		m.focus = 0
		cmds = append(cmds, m.send(e, FocusMsg{Focused: true}))
	}
	m.retarget()
	return tea.Batch(append(cmds, m.clock.Wake())...)
}

// Retires the card at index i by flying it off the screen edge
func (m *Model) Remove(i int) tea.Cmd {
	if i < 0 || i >= len(m.cards) {
		return nil
	}
	e := m.cards[i]
	if m.grab != nil && m.grab.e == e {
		m.letGo()
	}
	m.cards = slices.Delete(m.cards, i, i+1)
	m.leaving = append(m.leaving, e)
	e.rect.Set(float64(m.width+2), e.rect.Y.Target, e.rect.W.Target, e.rect.H.Target)
	m.hover = -1
	var cmds []tea.Cmd
	switch {
	case len(m.cards) == 0:
		m.focus = 0
		cmds = append(cmds, m.send(e, FocusMsg{}))
	case i < m.focus:
		m.focus--
	case i == m.focus:
		cmds = append(cmds, m.send(e, FocusMsg{}))
		m.focus = min(i, len(m.cards)-1)
		m.cards[m.focus].decode = 0
		cmds = append(cmds, m.send(m.cards[m.focus], FocusMsg{Focused: true}))
	}
	m.stackMemo = min(max(m.stackMemo, 1), max(len(m.cards)-1, 1))
	m.retarget()
	return tea.Batch(append(cmds, m.clock.Wake())...)
}

// Moves focus to card i and starts its title decode
func (m *Model) Focus(i int) tea.Cmd {
	if i < 0 || i >= len(m.cards) || i == m.focus {
		return nil
	}
	cmds := []tea.Cmd{m.send(m.cards[m.focus], FocusMsg{})}
	m.focus = i
	if i > 0 {
		m.stackMemo = i
	}
	m.cards[i].decode = 0
	cmds = append(cmds, m.send(m.cards[i], FocusMsg{Focused: true}))
	m.retarget()
	return tea.Batch(append(cmds, m.clock.Wake())...)
}

// Trades card i with the main slot, or opens the main card alone when i already is main
func (m *Model) Promote(i int) tea.Cmd {
	if i < 0 || i >= len(m.cards) {
		return nil
	}
	if i == 0 {
		return m.SetMode(Solo)
	}
	m.swap(0, i)
	m.stackMemo = i
	m.hover = -1
	return m.clock.Wake()
}

// Switches between the split layout and a solo card
func (m *Model) SetMode(mode Mode) tea.Cmd {
	if m.mode == mode {
		return nil
	}
	m.mode = mode
	m.hover = -1
	m.dragging = false
	m.letGo()
	m.retarget()
	return m.clock.Wake()
}

// Sets the main slot share of the width, clamped to keep both columns usable
func (m *Model) SetRatio(r float64) tea.Cmd {
	r = clampRatio(r)
	if r == m.ratio {
		return nil
	}
	m.ratio = r
	m.retarget()
	return m.clock.Wake()
}

// Resizes the deck, snaps placed cards to the new layout, and deals in cards still waiting
func (m *Model) SetSize(w, h int) {
	sized := m.width > 0 && m.height > 0
	m.width, m.height = max(w, 0), max(h, 0)
	placed := make([]bool, len(m.cards))
	for i, e := range m.cards {
		placed[i] = e.placed
	}
	m.retarget()
	if !sized {
		return
	}
	for i, e := range m.cards {
		if placed[i] && !e.held {
			e.rect.Snap()
		}
	}
}

// Current layout arrangement
func (m *Model) Mode() Mode { return m.mode }

// Index of the focused card
func (m *Model) Focused() int { return m.focus }

// Number of live cards
func (m *Model) Len() int { return len(m.cards) }

// Main slot share of the width
func (m *Model) Ratio() float64 { return m.ratio }

// Reports whether the hint bar and card ordinals are shown
func (m *Model) Hints() bool { return m.hints }

// Live cards in main then stack order
func (m *Model) Cards() []Card {
	out := make([]Card, len(m.cards))
	for i, e := range m.cards {
		out[i] = e.card
	}
	return out
}

// Reports whether the focused card is swallowing keys
func (m *Model) Capturing() bool {
	if len(m.cards) == 0 {
		return false
	}
	c, ok := m.cards[m.focus].card.(Capturer)
	return ok && c.Capturing()
}

// Advances every animation by dt seconds and reports whether anything is still moving
func (m *Model) Advance(dt float64) bool {
	m.dealt = 0
	moving := false
	for i, e := range m.cards {
		moving = m.step(e, dt, i == m.focus, i == m.hover) || moving
	}
	keep := m.leaving[:0]
	for _, e := range m.leaving {
		if m.step(e, dt, false, false) {
			keep = append(keep, e)
			moving = true
		}
	}
	clear(m.leaving[len(keep):])
	m.leaving = keep
	m.phase += dt * flowSpeed
	m.tick++
	return moving
}

func (m *Model) step(e *entry, dt float64, focused, hovered bool) bool {
	if e.delay > 0 {
		e.delay -= dt
		return true
	}
	moving := false
	if !e.held {
		moving = e.rect.Step(dt)
	}
	e.lift.Set(flag(e.held || e.rect.Distance() > liftDistance))
	moving = e.lift.Step(dt) || moving
	e.glow.Set(flag(focused))
	moving = e.glow.Step(dt) || moving
	e.hover.Set(flag(hovered))
	moving = e.hover.Step(dt) || moving
	if e.decode < 1 {
		e.decode = min(1, e.decode+dt/decodeTime)
		moving = true
	}
	return moving
}

// Recomputes resting boxes and starts unplaced cards from beyond the right edge
func (m *Model) retarget() {
	if m.width == 0 || m.height == 0 {
		return
	}
	m.boxes = layout(m.width, m.stageHeight(), len(m.cards), m.ratio, m.mode, m.focus, m.stackMemo)
	for i, e := range m.cards {
		b := m.boxes[i]
		if !e.placed {
			e.rect = anim.NewRect(float64(m.width+2), float64(b.y), float64(b.w), float64(b.h))
			e.delay = float64(m.dealt) * dealStagger
			e.placed = true
			m.dealt++
		}
		e.rect.Set(float64(b.x), float64(b.y), float64(b.w), float64(b.h))
	}
}

// Moves every placed card straight to its resting box
func (m *Model) snap() {
	for _, e := range m.cards {
		if e.placed && !e.held {
			e.rect.Snap()
		}
	}
}

// Trades the cards at a and b while focus stays on the same card
func (m *Model) swap(a, b int) {
	m.cards[a], m.cards[b] = m.cards[b], m.cards[a]
	switch m.focus {
	case a:
		m.focus = b
	case b:
		m.focus = a
	}
	if m.focus > 0 {
		m.stackMemo = m.focus
	}
	m.retarget()
}

// Releases any dragged card so it springs back into its slot
func (m *Model) letGo() {
	if m.grab == nil {
		return
	}
	m.grab.e.held = false
	m.grab, m.drop = nil, -1
}

func (m *Model) send(e *entry, msg tea.Msg) tea.Cmd {
	c, cmd := e.card.Update(msg)
	if c != nil {
		e.card = c
	}
	e.dirty = true
	return cmd
}

func (m *Model) broadcast(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.cards)+len(m.leaving))
	for _, e := range m.cards {
		cmds = append(cmds, m.send(e, msg))
	}
	for _, e := range m.leaving {
		cmds = append(cmds, m.send(e, msg))
	}
	return tea.Batch(cmds...)
}

func fast(v float64) anim.Value {
	x := anim.NewValue(v)
	x.Frequency, x.Damping = fastSpring, 1
	return x
}

func flag(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func clampRatio(r float64) float64 { return min(max(r, minRatio), maxRatio) }
