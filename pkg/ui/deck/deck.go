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
	pulseSpeed   = 0.35
)

type entry struct {
	id      int
	card    Card
	rect    anim.Rect
	glow    anim.Value
	hover   anim.Value
	lift    anim.Value
	pulse   anim.Value
	urgency Urgency
	decode  float64
	delay   float64
	dim     float64
	fade    float64
	placed  bool
	held    bool
	dirty   bool
	cw, ch  int
	raw     screen
	tinted  screen
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
	overlay   *entry
	toasts    []*toast
	sel       *selection
	status    *tea.ProgramStatus
	progress  *tea.ProgressBar
	focus     int
	stackMemo int
	drop      int
	hover     int
	hintHot   int
	ratio     float64
	motion    float64
	mode      Mode
	width     int
	height    int
	hints     bool
	ratioSet  bool
	hintsSet  bool
	themeSet  bool
	gutterHot bool
	dragging  bool
	blurred   bool
	resting   bool
	scrubX    int
	scrubbing bool
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

// Replaces the Blackwall palette and keeps a restored layout from changing it
func WithTheme(t theme.Theme) Option {
	return func(m *Model) { m.SetTheme(t); m.themeSet = true }
}

// Sets the share of the width given to the main slot and keeps a restored layout from changing it
func WithRatio(r float64) Option {
	return func(m *Model) { m.ratio, m.ratioSet = clampRatio(r), true }
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

// Shows or hides the key hint bar and card ordinals at startup and keeps a restored layout from changing it
func WithHints(show bool) Option {
	return func(m *Model) { m.hints, m.hintsSet = show, true }
}

// Scales every animation, one for full springs and zero for layouts that snap into place
func WithMotion(scale float64) Option {
	return func(m *Model) { m.SetMotion(scale) }
}

// Starts the deck in a layout mode
func WithMode(mode Mode) Option {
	return func(m *Model) { m.mode = mode }
}

// Builds a deck with the Blackwall theme, a 62 percent main slot, hints on, and 60 over 20 frame rates
func New(opts ...Option) *Model {
	m := &Model{
		Keys:      DefaultKeyMap(),
		theme:     theme.Blackwall(),
		clock:     anim.NewClock(60, 20),
		ratio:     0.62,
		motion:    1,
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
	cmds := append(m.pending, m.wake())
	m.pending = nil
	return tea.Batch(cmds...)
}

// Routes frames to the animation, keys to the focused card, and the mouse to the card under it
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, m.wake()
	case anim.FrameMsg:
		frame, ok := m.clock.Accept(msg)
		if !ok {
			return m, nil
		}
		moving := m.Advance(frame.DT)
		if !moving && m.canRest() {
			m.resting = true
			return m, nil
		}
		return m, m.clock.Next(moving)
	case tea.KeyPressMsg:
		return m, m.after(m.handleKey(msg))
	case tea.KeyReleaseMsg:
		return m, m.after(m.sendFocused(msg))
	case tea.PasteMsg, tea.PasteStartMsg, tea.PasteEndMsg:
		return m, m.after(m.sendFocused(msg))
	case tea.MouseMsg:
		return m, m.after(m.handleMouse(msg))
	case tea.FocusMsg:
		m.blurred = false
		return m, m.wake()
	case tea.BlurMsg:
		m.blurred = true
		return m, nil
	case Intent:
		return m, m.after(m.handleIntent(msg))
	case toastTickMsg:
		m.pruneToasts()
		return m, m.wake()
	}
	return m, m.after(m.broadcast(msg))
}

// Adds a frame request to a handler result when the clock had stopped, so input always repaints
func (m *Model) after(cmd tea.Cmd) tea.Cmd {
	if !m.resting {
		return cmd
	}
	wake := m.wake()
	if cmd == nil {
		return wake
	}
	return tea.Batch(cmd, wake)
}

// Reports whether the clock may stop between frames, which it may once the terminal is blurred or motion is off
func (m *Model) canRest() bool {
	if len(m.toasts) > 0 {
		return false
	}
	return m.blurred || m.motion == 0 || (len(m.cards) == 0 && m.overlay == nil)
}

// Reports whether the clock has stopped scheduling frames
func (m *Model) Resting() bool { return m.resting }

// Schedules a frame now and leaves the rest state
func (m *Model) wake() tea.Cmd {
	m.resting = false
	return m.clock.Wake()
}

// Deals a new card into the stack from the screen edge
func (m *Model) Add(c Card) tea.Cmd {
	return m.AddAfter(len(m.cards)-1, c)
}

// Deals a new card in right after position i, or at the end when i is past the last card
func (m *Model) AddAfter(i int, c Card) tea.Cmd {
	e := &entry{id: m.nextID, card: c, rect: anim.NewRect(0, 0, 0, 0), glow: fast(0), hover: fast(0), lift: fast(0), pulse: fast(0), dim: -1, dirty: true}
	m.nextID++
	at := min(max(i+1, 0), len(m.cards))
	m.cards = slices.Insert(m.cards, at, e)
	cmds := []tea.Cmd{c.Init()}
	if len(m.cards) == 1 {
		m.focus = 0
		cmds = append(cmds, m.send(e, FocusMsg{Focused: true}))
	} else if at <= m.focus {
		m.focus++
	}
	m.retarget()
	return tea.Batch(append(cmds, m.wake())...)
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
	if m.sel != nil && m.sel.e == e {
		m.sel = nil
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
	return tea.Batch(append(cmds, m.wake())...)
}

// Moves focus to card i, starts its title decode, and clears any attention it asked for
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
	m.cards[i].urgency = Calm
	cmds = append(cmds, m.send(m.cards[i], FocusMsg{Focused: true}))
	m.retarget()
	return tea.Batch(append(cmds, m.wake())...)
}

// Trades card i with the main slot, or opens the main card alone when i already is main, pinned cards staying put
func (m *Model) Promote(i int) tea.Cmd {
	if i < 0 || i >= len(m.cards) {
		return nil
	}
	if i == 0 {
		return m.SetMode(Solo)
	}
	if isPinned(m.cards[i].card) || isPinned(m.cards[0].card) {
		return m.nudge()
	}
	m.swap(0, i)
	m.stackMemo = i
	m.hover = -1
	return m.wake()
}

// Switches between the split layout, a solo card, and the timeline fan
func (m *Model) SetMode(mode Mode) tea.Cmd {
	if m.mode == mode {
		return nil
	}
	m.mode = mode
	m.hover = -1
	m.dragging = false
	m.scrubbing = false
	m.letGo()
	m.retarget()
	return m.wake()
}

// Sets the main slot share of the width, clamped to keep both columns usable
func (m *Model) SetRatio(r float64) tea.Cmd {
	r = clampRatio(r)
	if r == m.ratio {
		return nil
	}
	m.ratio = r
	m.retarget()
	return m.wake()
}

// Replaces the palette and border set, repainting every card
func (m *Model) SetTheme(t theme.Theme) {
	m.theme = t
	m.rings = map[int][]color.Color{}
	m.fade = map[color.Color]color.Color{}
	for _, e := range m.cards {
		e.dirty = true
	}
	if m.overlay != nil {
		m.overlay.dirty = true
	}
}

// Palette in use
func (m *Model) Theme() theme.Theme { return m.theme }

// Scales every animation, zero snapping layouts, decodes and the focus ring into place
func (m *Model) SetMotion(scale float64) {
	m.motion = min(max(scale, 0), 1)
	if m.motion == 0 {
		m.snap()
		for _, e := range m.cards {
			e.decode = 1
		}
	}
}

func (m *Model) Motion() float64 { return m.motion }

// Reports the program state to the terminal, nil clearing it
func (m *Model) SetStatus(ps *tea.ProgramStatus) { m.status = ps }

// Shows a progress bar in the terminal, nil clearing it
func (m *Model) SetProgress(pb *tea.ProgressBar) { m.progress = pb }

// Sets the terminal window title
func (m *Model) SetWindowTitle(title string) { m.title = title }

// Lights the frame of card i with an urgency until it is focused, Calm turning it off
func (m *Model) Attention(i int, u Urgency) tea.Cmd {
	if i < 0 || i >= len(m.cards) {
		return nil
	}
	e := m.cards[i]
	if i == m.focus && u != Calm {
		e.urgency = Calm
		return m.nudge()
	}
	e.urgency = u
	return m.wake()
}

// Urgency card i currently asks for
func (m *Model) UrgencyOf(i int) Urgency {
	if i < 0 || i >= len(m.cards) {
		return Calm
	}
	return m.cards[i].urgency
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
	m.placeOverlay()
	if !sized {
		return
	}
	for i, e := range m.cards {
		if placed[i] && !e.held {
			e.rect.Snap()
		}
	}
	if m.overlay != nil {
		m.overlay.rect.Snap()
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

// Screen size the deck was last given
func (m *Model) Size() (int, int) { return m.width, m.height }

// Live cards in main then stack order
func (m *Model) Cards() []Card {
	out := make([]Card, len(m.cards))
	for i, e := range m.cards {
		out[i] = e.card
	}
	return out
}

// Card at index i
func (m *Model) Card(i int) (Card, bool) {
	if i < 0 || i >= len(m.cards) {
		return nil, false
	}
	return m.cards[i].card, true
}

// Index of the card, or -1 when the deck does not hold it
func (m *Model) IndexOf(c Card) int { return m.indexOf(c) }

// Reports whether the focused card, or the overlay when one is up, is swallowing keys
func (m *Model) Capturing() bool {
	if m.overlay != nil {
		c, ok := m.overlay.card.(Capturer)
		return ok && c.Capturing()
	}
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
		moving = m.step(e, dt, i == m.focus && m.overlay == nil, i == m.hover) || moving
	}
	if m.overlay != nil {
		moving = m.step(m.overlay, dt, true, false) || moving
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
	moving = m.stepToasts(dt) || moving
	m.phase += dt * flowSpeed * m.motion
	m.tick++
	return moving
}

func (m *Model) step(e *entry, dt float64, focused, hovered bool) bool {
	if m.motion == 0 {
		e.delay = 0
		if !e.held {
			e.rect.Snap()
		}
		e.lift.Set(flag(e.held))
		e.lift.Snap()
		e.glow.Set(flag(focused))
		e.glow.Snap()
		e.hover.Set(flag(hovered))
		e.hover.Snap()
		e.pulse.Set(flag(e.urgency != Calm))
		e.pulse.Snap()
		e.decode = 1
		return false
	}
	if e.delay > 0 {
		e.delay -= dt
		return true
	}
	dt /= m.motion
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
	e.pulse.Set(flag(e.urgency != Calm))
	moving = e.pulse.Step(dt) || moving
	if e.urgency != Calm {
		moving = true
	}
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
	cons := make([]constraint, len(m.cards))
	for i, e := range m.cards {
		cons[i] = constraintOf(e.card)
	}
	m.boxes = layoutWith(m.width, m.stageHeight(), len(m.cards), m.ratio, m.mode, m.focus, m.stackMemo, cons)
	for i, e := range m.cards {
		b := m.boxes[i]
		e.fade = 0
		if m.mode == Timeline {
			d := i - m.focus
			if d < 0 {
				d = -d
			}
			e.fade = min(0.6, float64(d)*0.15)
		}
		if !e.placed {
			e.rect = anim.NewRect(float64(m.width+2), float64(b.y), float64(b.w), float64(b.h))
			e.delay = float64(m.dealt) * dealStagger * m.motion
			e.placed = true
			m.dealt++
		}
		e.rect.Set(float64(b.x), float64(b.y), float64(b.w), float64(b.h))
		if m.motion == 0 {
			e.rect.Snap()
		}
	}
}

// Moves every placed card straight to its resting box
func (m *Model) snap() {
	for _, e := range m.cards {
		if e.placed && !e.held {
			e.rect.Snap()
		}
	}
	if m.overlay != nil {
		m.overlay.rect.Snap()
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

// Delivers a message to the overlay when one is up and otherwise to the focused card
func (m *Model) sendFocused(msg tea.Msg) tea.Cmd {
	if m.overlay != nil {
		return m.send(m.overlay, msg)
	}
	if len(m.cards) == 0 {
		return nil
	}
	return m.send(m.cards[m.focus], msg)
}

func (m *Model) broadcast(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.cards)+len(m.leaving)+1)
	for _, e := range m.cards {
		cmds = append(cmds, m.send(e, msg))
	}
	for _, e := range m.leaving {
		cmds = append(cmds, m.send(e, msg))
	}
	if m.overlay != nil {
		cmds = append(cmds, m.send(m.overlay, msg))
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
