package wm

import (
	"image/color"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

const (
	barHeight     = 1
	popShare      = 0.08
	closeShare    = 0.06
	originShare   = 0.4
	decodeTime    = 0.35
	flowSpeed     = 16.0
	nudgeImpulse  = 90.0
	doubleClick   = 350 * time.Millisecond
	fastSpring    = 18.0
	pulseSpeed    = 0.35
	contentDim    = 0.3
	backdropDim   = 0.45
	slideFade     = 0.55
	maxFloatPile  = 8
	maxToasts     = 5
	toastGap      = 2
	cascadeSpeed  = 6.0
	cascadeTail   = 6.0
	cascadeBreath = 0.85
	gripReach     = 5.0
)

// Card picked up by its title, with the pointer offset inside it and whether it has moved yet
type grab struct {
	e      *entry
	dx, dy int
	x, y   int
	moved  bool
}

// Edge being dragged to resize, with the tile beyond it in a stack or the spiral level it cuts
type resizer struct {
	e     *entry
	edge  region
	other *entry
	split *node
}

// Window manager that tiles cards per workspace, floats dialogs, animates every move and routes input
type Model struct {
	theme     theme.Theme
	clock     anim.Clock
	spaces    []*Workspace
	active    int
	leaving   *Workspace
	overlay   *entry
	parting   *entry
	veil      anim.Value
	toasts    []*toast
	onToast   func(text string, level Level)
	sel       *selection
	grab      *grab
	resizer   *resizer
	hover     *entry
	hoverEdge region
	hoverX    int
	hoverY    int
	hintHot   string
	hot       barSpot
	commands  []Command
	keys      *keys.Map
	recent    []string
	empty     []string
	hold      int
	report    func() Status
	notice    Notice
	dials     []dial
	motion    float64
	cascade   bool
	gap       int
	gapOutX   int
	gapOutY   int
	padX      int
	padY      int
	ratio     float64
	mode      Mode
	main      int
	ratioSet  bool
	motionSet bool
	modeSet   bool
	hoverGrip bool
	width     int
	height    int
	blurred   bool
	resting   bool
	phase     float64
	tick      int
	nextID    int
	title     string
	status    *tea.ProgramStatus
	progress  *tea.ProgressBar
	now       func() time.Time
	lastClick time.Time
	clickID   int
	chipX     anim.Value
	chipW     anim.Value
	chips     []chip
	fieldX    anim.Value
	fieldW    anim.Value
	caret     int
	rings     map[int][]color.Color
	glyphs    map[string]uv.Cell
	memoed    map[color.Color]color.Color
	card      screen
	buf       screen
}

// Configures the manager at construction
type Option func(*Model)

// Replaces the Blackwall palette
func WithTheme(t theme.Theme) Option {
	return func(m *Model) { m.SetTheme(t) }
}

// Scales every animation, one for full springs and zero for layouts that snap into place
func WithMotion(scale float64) Option {
	return func(m *Model) { m.SetMotion(scale); m.motionSet = true }
}

// Lights the focused card's border with a flowing energy comet
func WithCascade(on bool) Option {
	return func(m *Model) { m.SetCascade(on) }
}

// Sets the terminal window title
func WithWindowTitle(title string) Option {
	return func(m *Model) { m.title = title }
}

// Sets the share of the stage new workspaces give the master area and keeps a restored layout from changing it
func WithRatio(r float64) Option {
	return func(m *Model) { m.ratio, m.ratioSet = clampRatio(r), true }
}

// Replaces the keymap keys are dispatched and legends painted from
func WithKeys(km *keys.Map) Option {
	return func(m *Model) { m.SetKeys(km) }
}

// Builds a manager with the Blackwall theme, one cell gaps, a two column stage margin and 60 over 30 frames
func New(opts ...Option) *Model {
	m := &Model{
		clock:   anim.NewClock(60, 30),
		motion:  1,
		gap:     1,
		gapOutX: 2,
		gapOutY: 1,
		ratio:   defaultRatio,
		main:    1,
		now:     time.Now,
		keys:    keys.Default(),
		empty:   []string{"palette"},
		chipX:   fast(0),
		chipW:   fast(0),
		fieldX:  fast(0),
		fieldW:  fast(0),
		caret:   -1,
		veil:    anim.NewValueWith(0, anim.Gentle),
		rings:   map[int][]color.Color{},
		glyphs:  map[string]uv.Cell{},
		memoed:  map[color.Color]color.Color{},
	}
	m.SetTheme(theme.Blackwall())
	for _, o := range opts {
		o(m)
	}
	return m
}

// Starts the frame clock
func (m *Model) Init() tea.Cmd { return m.wake() }

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
	case tea.KeyReleaseMsg, tea.PasteMsg, tea.PasteStartMsg, tea.PasteEndMsg:
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
	return m.blurred || m.motion == 0
}

// Reports whether the clock has stopped scheduling frames
func (m *Model) Resting() bool { return m.resting }

// Schedules a frame now and leaves the rest state
func (m *Model) wake() tea.Cmd {
	m.resting = false
	return m.clock.Wake()
}

// Workspace shown on the stage
func (m *Model) Active() *Workspace {
	if m.active < 0 || m.active >= len(m.spaces) {
		return nil
	}
	return m.spaces[m.active]
}

// Index of the workspace shown on the stage
func (m *Model) ActiveIndex() int { return m.active }

// Workspaces in order
func (m *Model) Workspaces() []*Workspace { return append([]*Workspace(nil), m.spaces...) }

// Workspace bound to the id, nil when none is
func (m *Model) WorkspaceByID(id string) *Workspace {
	for _, ws := range m.spaces {
		if ws.ID == id {
			return ws
		}
	}
	return nil
}

// Index of the workspace, -1 when it is not held
func (m *Model) IndexOf(ws *Workspace) int { return slices.Index(m.spaces, ws) }

// Adds a workspace tiling the cards in order with the first focused, showing it when it is the first workspace
func (m *Model) AddWorkspace(id string, cards ...Card) (*Workspace, tea.Cmd) {
	ws := &Workspace{ID: id, ratio: m.ratio, mode: m.mode, main: m.main, shift: fast(0)}
	m.spaces = append(m.spaces, ws)
	var cmds []tea.Cmd
	for _, c := range cards {
		if c == nil {
			continue
		}
		e := m.newEntry(c, ws)
		ws.addTile(e)
		if ws.focus == nil {
			ws.focus = e
		}
		cmds = append(cmds, c.Init())
	}
	if len(m.spaces) == 1 {
		m.active = 0
		if ws.focus != nil {
			cmds = append(cmds, m.send(ws.focus, FocusMsg{Focused: true}))
		}
	}
	m.retarget(ws)
	m.layoutBar()
	return ws, tea.Batch(append(cmds, m.wake())...)
}

// Shows workspace i, sliding the stage toward it
func (m *Model) Switch(i int) tea.Cmd {
	if i < 0 || i >= len(m.spaces) || i == m.active {
		return m.nudge()
	}
	old := m.Active()
	dir := 1.0
	if i < m.active {
		dir = -1
	}
	var cmds []tea.Cmd
	if old != nil {
		m.letGo()
		m.sel = nil
		if old.focus != nil {
			cmds = append(cmds, m.send(old.focus, FocusMsg{}))
		}
		old.shift.Set(-dir * float64(m.width+2))
		m.leaving = old
	}
	m.active = i
	ws := m.spaces[i]
	ws.urgent = false
	ws.shift.Pos, ws.shift.Vel = dir*float64(m.width+2), 0
	ws.shift.Set(0)
	if m.motion == 0 {
		ws.shift.Snap()
		m.leaving = nil
	}
	m.hover = nil
	if ws.focus == nil && len(ws.tiles) > 0 {
		ws.focus = ws.tiles[0]
	}
	if ws.focus != nil {
		ws.focus.decode = 0
		cmds = append(cmds, m.send(ws.focus, FocusMsg{Focused: true}))
	}
	m.retarget(ws)
	m.layoutBar()
	return tea.Batch(append(cmds, m.wake())...)
}

// Drops workspace i and its cards, showing a neighbor when it was on the stage
func (m *Model) RemoveWorkspace(i int) tea.Cmd {
	if i < 0 || i >= len(m.spaces) || len(m.spaces) == 1 {
		return m.nudge()
	}
	ws := m.spaces[i]
	var cmds []tea.Cmd
	if i == m.active {
		next := i - 1
		if next < 0 {
			next = 1
		}
		cmds = append(cmds, m.Switch(next))
	}
	for _, e := range ws.entries() {
		cmds = append(cmds, m.send(e, FocusMsg{}), m.send(e, ClosedMsg{}))
	}
	m.spaces = slices.Delete(m.spaces, i, i+1)
	if m.leaving == ws {
		m.leaving = nil
	}
	if m.active > i {
		m.active--
	}
	m.layoutBar()
	return tea.Batch(append(cmds, m.wake())...)
}

// Opens the card in the workspace, tiled or floating as it asks, or focuses the one with its key already open
func (m *Model) Open(ws *Workspace, c Card) tea.Cmd { return m.OpenFrom(ws, c, Box{}) }

// Opens the card the way Open does, growing it out of the origin box when one is given
func (m *Model) OpenFrom(ws *Workspace, c Card, origin Box) tea.Cmd {
	if ws == nil || c == nil {
		return nil
	}
	if have := ws.find(keyOf(c)); have != nil {
		return m.focusEntry(have)
	}
	for _, other := range m.spaces {
		if have := other.find(keyOf(c)); have != nil && other != ws {
			return m.relocate(have, ws)
		}
	}
	e := m.adopt(ws, c, origin)
	cmds := []tea.Cmd{c.Init(), m.focusEntry(e)}
	m.retarget(ws)
	return tea.Batch(append(cmds, m.wake())...)
}

// Opens the card in a hidden workspace without switching to it, the card waiting there as its focus
func (m *Model) OpenAside(ws *Workspace, c Card, origin Box) tea.Cmd {
	if ws == nil || c == nil || ws == m.Active() {
		return m.OpenFrom(ws, c, origin)
	}
	var cmds []tea.Cmd
	e := ws.find(keyOf(c))
	for _, other := range m.spaces {
		if have := other.find(keyOf(c)); e == nil && have != nil {
			e = have
			wasFocus := other.focus == have
			cmds = m.carry(have, ws)
			if wasFocus && other == m.Active() && other.focus != nil {
				other.focus.decode = 0
				cmds = append(cmds, m.send(other.focus, FocusMsg{Focused: true}))
			}
		}
	}
	if e == nil {
		e = m.adopt(ws, c, origin)
		cmds = append(cmds, c.Init())
	}
	ws.focus = e
	if e.floating {
		ws.raise(e)
	} else if ws.zoom != nil && ws.zoom != e {
		ws.zoom = nil
	}
	m.retarget(ws)
	return tea.Batch(append(cmds, m.wake())...)
}

// Places a new card in the workspace, floating or tiled as it asks, grown from the origin box
func (m *Model) adopt(ws *Workspace, c Card, origin Box) *entry {
	e := m.newEntry(c, ws)
	e.origin = origin
	if !floats(c) {
		ws.addTile(e)
		return e
	}
	e.floating = true
	e.fbox = m.floatBox(c, Box{})
	ws.addFloat(e)
	if len(ws.floats) > maxFloatPile {
		ws.floats = ws.floats[len(ws.floats)-maxFloatPile:]
	}
	return e
}

// Opens the card in the workspace on the stage
func (m *Model) OpenHere(c Card) tea.Cmd { return m.Open(m.Active(), c) }

// Closes the card wherever it is open
func (m *Model) Close(c Card) tea.Cmd {
	if e := m.entryOf(c); e != nil {
		return m.closeEntry(e)
	}
	return nil
}

// Closes the card with the key wherever it is open
func (m *Model) CloseKey(key string) tea.Cmd {
	for _, ws := range m.spaces {
		if e := ws.find(key); e != nil {
			return m.closeEntry(e)
		}
	}
	return nil
}

// Closes the focused card when it allows closing
func (m *Model) CloseFocused() tea.Cmd {
	e := m.focused()
	if e == nil || !closable(e.card) {
		return m.nudge()
	}
	return m.closeEntry(e)
}

// Focuses the card, switching to its workspace when it is in another
func (m *Model) Focus(c Card) tea.Cmd {
	e := m.entryOf(c)
	if e == nil {
		return nil
	}
	return m.focusEntry(e)
}

// Box the frame of the card is painted in on its stage right now, false when it is not open
func (m *Model) FrameOf(c Card) (Box, bool) {
	e := m.entryOf(c)
	if e == nil || !e.placed {
		return Box{}, false
	}
	return e.frame(), true
}

// Card with the key and the workspace holding it
func (m *Model) Find(key string) (Card, *Workspace) {
	for _, ws := range m.spaces {
		if e := ws.find(key); e != nil {
			return e.card, ws
		}
	}
	return nil, nil
}

// Reports whether a card with the key is open anywhere
func (m *Model) Has(key string) bool {
	c, _ := m.Find(key)
	return c != nil
}

// Card with the input focus on the stage
func (m *Model) Focused() Card {
	if e := m.focused(); e != nil {
		return e.card
	}
	return nil
}

// Workspace holding the card, nil when it is not open
func (m *Model) WorkspaceOf(c Card) *Workspace {
	if e := m.entryOf(c); e != nil {
		return e.ws
	}
	return nil
}

// Moves focus to the nearest tile in the direction, floats first stepping back to the tiles
func (m *Model) FocusDir(d Dir) tea.Cmd {
	ws := m.Active()
	e := m.focused()
	if ws == nil || e == nil {
		return nil
	}
	if ws.zoom != nil {
		return m.nudge()
	}
	if e.floating {
		if len(ws.tiles) == 0 {
			return m.nudge()
		}
		return m.focusEntry(ws.tiles[0])
	}
	n := ws.neighbor(e, d)
	if n == nil {
		return m.nudge()
	}
	return m.focusEntry(n)
}

// Moves focus k steps through the tiles and floats of the stage, wrapping at both ends
func (m *Model) Cycle(k int) tea.Cmd {
	ws := m.Active()
	if ws == nil {
		return nil
	}
	all := ws.entries()
	if len(all) < 2 {
		return m.nudge()
	}
	i := slices.Index(all, ws.focus)
	return m.focusEntry(all[((i+k)%len(all)+len(all))%len(all)])
}

// Trades the focused tile with its neighbor in the direction, or nudges a float by a few cells
func (m *Model) Move(d Dir) tea.Cmd {
	ws := m.Active()
	e := m.focused()
	if ws == nil || e == nil {
		return nil
	}
	if e.floating {
		dx, dy := 0, 0
		switch d {
		case Left:
			dx = -4
		case Right:
			dx = 4
		case Up:
			dy = -2
		case Down:
			dy = 2
		}
		e.fbox.X += dx
		e.fbox.Y += dy
		m.retarget(ws)
		return m.wake()
	}
	n := ws.neighbor(e, d)
	if n == nil {
		return m.nudge()
	}
	ws.swap(e, n)
	m.retarget(ws)
	return m.wake()
}

// Trades the focused tile with the master, or the master with the first stack tile
func (m *Model) SwapMaster() tea.Cmd {
	ws := m.Active()
	e := m.focused()
	if ws == nil || e == nil || len(ws.tiles) < 2 || e.floating {
		return m.nudge()
	}
	other := ws.tiles[0]
	if e == other {
		other = ws.tiles[1]
	}
	ws.swap(e, other)
	m.retarget(ws)
	return m.wake()
}

// Fills the stage with the focused card or gives the stage back
func (m *Model) ToggleZoom() tea.Cmd {
	ws := m.Active()
	e := m.focused()
	if ws == nil || e == nil {
		return nil
	}
	return m.setZoom(e, ws.zoom != e)
}

// Floats the focused tile or tiles the focused float
func (m *Model) ToggleFloat() tea.Cmd {
	e := m.focused()
	if e == nil {
		return nil
	}
	return m.setFloat(e, !e.floating)
}

// Changes the master share of the stage by delta, or the split the focused tile was cut off by
func (m *Model) Resize(delta float64) tea.Cmd {
	ws := m.Active()
	if ws == nil {
		return nil
	}
	if ws.mode == Dwindle {
		n := ws.tree.find(m.focused())
		if n == nil || n.parent == nil {
			return m.nudge()
		}
		if n.parent.a == n {
			n.parent.ratio = clampSplit(clampSplit(n.parent.ratio) + delta)
		} else {
			n.parent.ratio = clampSplit(clampSplit(n.parent.ratio) - delta)
		}
		m.retarget(ws)
		return m.wake()
	}
	r := clampRatio(ws.ratio + delta)
	if r == ws.ratio {
		return m.nudge()
	}
	ws.ratio = r
	m.retarget(ws)
	return m.wake()
}

// Flips the cut that holds the focused tile between side by side and stacked
func (m *Model) FlipSplit() tea.Cmd {
	ws := m.Active()
	e := m.focused()
	if ws == nil || e == nil || ws.mode != Dwindle || !ws.flipSplit(e) {
		return m.nudge()
	}
	m.retarget(ws)
	return m.wake()
}

// Changes how many tiles the main area of the master arrangement holds
func (m *Model) SetMainCount(delta int) tea.Cmd {
	ws := m.Active()
	if ws == nil || ws.mode != Master {
		return m.nudge()
	}
	next := min(max(ws.MainCount()+delta, 1), max(len(ws.live()), 1))
	if next == ws.MainCount() {
		return m.nudge()
	}
	ws.main = next
	m.retarget(ws)
	return m.wake()
}

// Switches the workspace on the stage to an arrangement
func (m *Model) SetLayout(mode Mode) tea.Cmd {
	ws := m.Active()
	if ws == nil || ws.mode == mode {
		return nil
	}
	ws.mode = mode
	if mode == Dwindle {
		ws.rebuildTree(m.stage(), m.gap)
	} else {
		ws.syncTiles()
		ws.tree = nil
	}
	m.retarget(ws)
	return m.wake()
}

// Box the tiles are laid out in, the stage inside the outer gaps
func (m *Model) stage() Box {
	sw, sh := m.width, m.stageHeight()
	if sw-2*m.gapOutX < minCols || sh-2*m.gapOutY < minRows {
		return Box{0, 0, sw, sh}
	}
	return Box{m.gapOutX, m.gapOutY, sw - 2*m.gapOutX, sh - 2*m.gapOutY}
}

// Switches the workspace on the stage between the master and dwindle arrangements
func (m *Model) ToggleLayout() tea.Cmd {
	ws := m.Active()
	if ws == nil {
		return nil
	}
	if ws.mode == Dwindle {
		return m.SetLayout(Master)
	}
	return m.SetLayout(Dwindle)
}

// Returns the master share, every split and every stack weight to their defaults
func (m *Model) ResetLayout() tea.Cmd {
	ws := m.Active()
	if ws == nil {
		return nil
	}
	ws.ratio = m.ratio
	ws.main = m.main
	ws.tree.even()
	for _, e := range ws.tiles {
		e.weight = 1
	}
	m.retarget(ws)
	return m.wake()
}

// Master share of the stage the workspace on the stage uses
func (m *Model) Ratio() float64 {
	if ws := m.Active(); ws != nil {
		return ws.ratio
	}
	return m.ratio
}

// Sets the master share of the workspace on the stage
func (m *Model) SetRatio(r float64) tea.Cmd {
	ws := m.Active()
	if ws == nil {
		return nil
	}
	ws.ratio = clampRatio(r)
	m.retarget(ws)
	return m.wake()
}

// Lights the frame of the card with an urgency until it is focused, Calm turning it off
func (m *Model) Attention(c Card, u Urgency) tea.Cmd {
	e := m.entryOf(c)
	if e == nil {
		return nil
	}
	return m.attention(e, u)
}

func (m *Model) attention(e *entry, u Urgency) tea.Cmd {
	if e == m.focused() && u != Calm {
		e.urgency = Calm
		return m.nudge()
	}
	e.urgency = u
	if u != Calm && e.ws != m.Active() {
		e.ws.urgent = true
	}
	return m.wake()
}

// Urgency the card asks for
func (m *Model) UrgencyOf(c Card) Urgency {
	if e := m.entryOf(c); e != nil {
		return e.urgency
	}
	return Calm
}

// Replaces the palette, border set and padding, repainting every card
func (m *Model) SetTheme(t theme.Theme) {
	m.theme = t
	m.padX, m.padY = 2, 1
	m.rings = map[int][]color.Color{}
	m.memoed = map[color.Color]color.Color{}
	m.Repaint()
	for _, e := range []*entry{m.overlay, m.parting} {
		if e == nil {
			continue
		}
		if p, ok := e.card.(*Palette); ok {
			p.theme = t
		}
	}
}

// Draws every card again on the next render, for state they show that changed outside them
func (m *Model) Repaint() {
	for _, ws := range m.spaces {
		for _, e := range ws.entries() {
			e.dirty = true
		}
	}
	if m.overlay != nil {
		m.overlay.dirty = true
	}
	if m.parting != nil {
		m.parting.dirty = true
	}
}

// Palette in use
func (m *Model) Theme() theme.Theme { return m.theme }

// Scales every animation, zero snapping layouts, decodes and the focus ring into place
func (m *Model) SetMotion(scale float64) {
	m.motion = min(max(scale, 0), 1)
	if m.motion == 0 {
		m.snap()
	}
}

func (m *Model) Motion() float64 { return m.motion }

// Turns the flowing energy comet on the focused card's border on or off
func (m *Model) SetCascade(on bool) { m.cascade = on }

// Reports whether the flowing energy comet is enabled
func (m *Model) Cascade() bool { return m.cascade }

// Reports the program state to the terminal, nil clearing it
func (m *Model) SetStatus(ps *tea.ProgramStatus) { m.status = ps }

// Shows a progress bar in the terminal, nil clearing it
func (m *Model) SetProgress(pb *tea.ProgressBar) { m.progress = pb }

// Sets the terminal window title
func (m *Model) SetWindowTitle(title string) { m.title = title }

// Supplies what the bar shows each frame, the lane of every workspace, the waiting item and the dials
func (m *Model) SetBar(fn func() Status) { m.report = fn }

// Actions an empty stage invites with, each shown with its key and label while bound
func (m *Model) SetEmptyHints(ids ...string) { m.empty = ids }

// Frames a size change has to settle for before cards draw themselves at their new size
const resizeHold = 6

// Resizes the stage, snapping placed cards to their new boxes, which keep their drawing while more sizes may follow
func (m *Model) SetSize(w, h int) {
	if (w != m.width || h != m.height) && m.motion > 0 {
		m.hold = resizeHold
	}
	m.width, m.height = max(w, 0), max(h, 0)
	for _, ws := range m.spaces {
		m.retarget(ws)
		for _, e := range ws.entries() {
			if e.placed && !e.held && !e.closing {
				e.rect.Snap()
			}
		}
		ws.shift.Snap()
	}
	m.leaving = nil
	m.parting = nil
	m.veil.Snap()
	m.placeOverlay()
	if m.overlay != nil {
		m.overlay.rect.Snap()
	}
	m.layoutBar()
	m.chipX.Snap()
	m.chipW.Snap()
	m.fieldX.Snap()
	m.fieldW.Snap()
}

// Screen size the manager was last given
func (m *Model) Size() (int, int) { return m.width, m.height }

// Reports whether the screen changed size a few frames ago, while more size changes may follow
func (m *Model) settling() bool { return m.hold > 0 }

func (m *Model) stageHeight() int { return max(m.height-barHeight, 0) }

// Reports whether an overlay holds the input
func (m *Model) Capturing() bool { return m.overlay != nil }

func (m *Model) focused() *entry {
	ws := m.Active()
	if ws == nil {
		return nil
	}
	return ws.focus
}

func (m *Model) entryOf(c Card) *entry {
	if c == nil {
		return nil
	}
	for _, ws := range m.spaces {
		if e := ws.holds(c); e != nil {
			return e
		}
	}
	return nil
}

func (m *Model) newEntry(c Card, ws *Workspace) *entry {
	e := &entry{id: m.nextID, card: c, ws: ws, rect: anim.NewRect(0, 0, 0, 0), open: fast(0), lit: fast(0), hover: fast(0), pulse: fast(0), hide: fast(0), shake: anim.NewValueWith(0, anim.Rattle), weight: 1, dim: -1, dirty: true}
	m.nextID++
	e.open.Set(1)
	return e
}

// Moves focus to the entry, switching to its workspace and raising it when it floats
func (m *Model) focusEntry(e *entry) tea.Cmd {
	var cmds []tea.Cmd
	if i := slices.Index(m.spaces, e.ws); i >= 0 && i != m.active {
		cmds = append(cmds, m.Switch(i))
	}
	ws := e.ws
	if ws.focus == e {
		if e.floating {
			ws.raise(e)
		}
		return tea.Batch(append(cmds, m.wake())...)
	}
	if ws.focus != nil {
		cmds = append(cmds, m.send(ws.focus, FocusMsg{}))
	}
	ws.focus = e
	e.decode = 0
	e.urgency = Calm
	if e.floating {
		ws.raise(e)
	}
	if ws.zoom != nil && ws.zoom != e && !e.floating {
		ws.zoom = nil
		m.retarget(ws)
	}
	cmds = append(cmds, m.send(e, FocusMsg{Focused: true}))
	return tea.Batch(append(cmds, m.wake())...)
}

// Starts the pop out of an entry, dropping it once the motion settles
func (m *Model) closeEntry(e *entry) tea.Cmd {
	if e.closing {
		return nil
	}
	ws := e.ws
	if m.grab != nil && m.grab.e == e {
		m.letGo()
	}
	if m.sel != nil && m.sel.e == e {
		m.sel = nil
	}
	if m.hover == e {
		m.hover = nil
	}
	e.closing = true
	e.held = false
	e.open.Set(0)
	ws.removeLeaf(e)
	cmds := []tea.Cmd{m.send(e, FocusMsg{}), m.send(e, ClosedMsg{})}
	wasFocus := ws.focus == e
	if ws.zoom == e {
		ws.zoom = nil
	}
	if wasFocus {
		ws.focus = nil
		if next := m.nextFocus(ws, e); next != nil {
			ws.focus = next
			next.decode = 0
			if ws == m.Active() {
				cmds = append(cmds, m.send(next, FocusMsg{Focused: true}))
			}
		}
	}
	if m.motion == 0 {
		m.drop(e)
	} else {
		m.retarget(ws)
	}
	return tea.Batch(append(cmds, m.wake())...)
}

// Entry that takes focus when gone closes, the top float, else the nearest tile below before above
func (m *Model) nextFocus(ws *Workspace, gone *entry) *entry {
	for i := len(ws.floats) - 1; i >= 0; i-- {
		if e := ws.floats[i]; e != gone && !e.closing {
			return e
		}
	}
	i := slices.Index(ws.tiles, gone)
	for d := 0; d < len(ws.tiles); d++ {
		for _, j := range []int{i + d, i - d} {
			if j >= 0 && j < len(ws.tiles) && ws.tiles[j] != gone && !ws.tiles[j].closing {
				return ws.tiles[j]
			}
		}
	}
	return nil
}

// Removes a closed entry from its workspace and lays the rest out again
func (m *Model) drop(e *entry) {
	e.ws.remove(e)
	m.retarget(e.ws)
}

func (m *Model) setZoom(e *entry, on bool) tea.Cmd {
	ws := e.ws
	if e.floating || e.closing {
		return m.nudge()
	}
	if on {
		ws.zoom = e
	} else if ws.zoom == e {
		ws.zoom = nil
	} else {
		return nil
	}
	m.retarget(ws)
	return tea.Batch(m.focusEntry(e), m.wake())
}

func (m *Model) setFloat(e *entry, on bool) tea.Cmd {
	ws := e.ws
	if e.floating == on || e.closing {
		return nil
	}
	ws.remove(e)
	e.floating = on
	if on {
		e.fbox = m.floatBox(e.card, e.frame())
		ws.addFloat(e)
	} else {
		ws.addTile(e)
	}
	ws.focus = e
	m.retarget(ws)
	return m.wake()
}

// Box a float opens at, inside the stage, from the card's size or the frame it had as a tile
func (m *Model) floatBox(c Card, from Box) Box {
	sw, sh := m.width, m.stageHeight()
	w, h := minSize(c)
	if !from.empty() {
		w, h = max(w, from.W*9/10), max(h, from.H*9/10)
	}
	if w <= 0 {
		w = sw * 3 / 5
	}
	if h <= 0 {
		h = sh * 3 / 5
	}
	w, h = min(max(w+2+2*m.padX, 12), sw), min(max(h+2+2*m.padY, 4), sh)
	x, y := (sw-w)/2, (sh-h)/2
	if !from.empty() {
		x, y = from.X+(from.W-w)/2, from.Y+(from.H-h)/2
	}
	return clampBox(Box{x, y, w, h}, sw, sh)
}

func clampBox(b Box, sw, sh int) Box {
	b.W, b.H = min(max(b.W, 4), max(sw, 4)), min(max(b.H, 3), max(sh, 3))
	b.X = min(max(b.X, 0), max(sw-b.W, 0))
	b.Y = min(max(b.Y, 0), max(sh-b.H, 0))
	return b
}

// Recomputes the resting boxes of a workspace and starts new cards from their pop in position
func (m *Model) retarget(ws *Workspace) {
	if m.width == 0 || m.height == 0 {
		return
	}
	sw, sh := m.width, m.stageHeight()
	live := ws.live()
	weights := make([]float64, 0, len(live))
	for _, e := range live {
		weights = append(weights, e.weight)
	}
	st := m.stage()
	ox, oy, iw, ih := st.X, st.Y, st.W, st.H
	if ws.mode == Dwindle && ws.tree != nil {
		ws.tree.layout(Box{ox, oy, iw, ih}, m.gap)
	} else {
		boxes := arrange(iw, ih, len(live), ws.ratio, weights, m.gap, ws.MainCount())
		for i, e := range live {
			e.box = boxes[i]
			e.box.X += ox
			e.box.Y += oy
		}
	}
	if ws.zoom != nil {
		ws.zoom.box = Box{0, 0, sw, sh}
	}
	for _, e := range ws.floats {
		e.fbox = clampBox(e.fbox, sw, sh)
		e.box = e.fbox
	}
	for _, e := range ws.entries() {
		m.aim(e)
	}
	ws.placed = true
}

// Points the frame of an entry at its box, grown from its origin when new and shrunk when closing
func (m *Model) aim(e *entry) {
	b := e.box
	if !e.placed {
		x, y, w, h := shrunk(b, popShare)
		if !e.origin.empty() {
			x, y, w, h = grown(e.origin, b, originShare)
		}
		e.rect = anim.NewRect(x, y, w, h)
		e.placed = true
	}
	if e.closing {
		cx, cy, cw, ch := shrunk(e.box, closeShare)
		e.rect.Set(cx, cy, cw, ch)
	} else if !e.held {
		e.rect.Set(float64(b.X), float64(b.Y), float64(b.W), float64(b.H))
	}
	if m.motion == 0 && !e.held {
		e.rect.Snap()
		e.open.Snap()
		e.decode = 1
	}
}

// Moves every placed card straight to its resting box
func (m *Model) snap() {
	for _, ws := range m.spaces {
		for _, e := range ws.entries() {
			if e.placed && !e.held {
				e.rect.Snap()
				e.open.Snap()
				e.decode = 1
			}
			e.shake.Snap()
		}
		ws.shift.Snap()
	}
	m.leaving = nil
	m.parting = nil
	m.veil.Snap()
	if m.overlay != nil {
		m.overlay.rect.Snap()
		m.overlay.open.Snap()
		m.overlay.shake.Snap()
	}
	m.chipX.Snap()
	m.chipW.Snap()
	m.fieldX.Snap()
	m.fieldW.Snap()
}

// Rattles the overlay or the focused frame sideways so an impossible action is felt
func (m *Model) nudge() tea.Cmd {
	e := m.overlay
	if e == nil {
		e = m.focused()
	}
	if e == nil || m.motion == 0 {
		return nil
	}
	e.shake.Kick(nudgeImpulse * m.motion)
	return m.wake()
}

// Releases any dragged card so it springs back into its slot
func (m *Model) letGo() {
	if m.grab != nil {
		m.grab.e.held = false
		m.grab = nil
	}
	m.resizer = nil
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
	if e := m.focused(); e != nil {
		return m.send(e, msg)
	}
	return nil
}

// Delivers a message to every card in every workspace and the overlay
func (m *Model) broadcast(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for _, ws := range m.spaces {
		for _, e := range ws.entries() {
			cmds = append(cmds, m.send(e, msg))
		}
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

// Moves an open card into another workspace, popping it in there with the focus
func (m *Model) relocate(e *entry, ws *Workspace) tea.Cmd {
	cmds := append(m.carry(e, ws), m.focusEntry(e))
	return tea.Batch(append(cmds, m.wake())...)
}

// Takes an open card out of its workspace and pops it into another, handing the old focus on
func (m *Model) carry(e *entry, ws *Workspace) []tea.Cmd {
	old := e.ws
	var cmds []tea.Cmd
	if old.focus == e {
		cmds = append(cmds, m.send(e, FocusMsg{}))
		old.focus = m.nextFocus(old, e)
	}
	old.remove(e)
	m.retarget(old)
	e.ws = ws
	e.placed = false
	e.held = false
	if e.floating {
		e.fbox = m.floatBox(e.card, Box{})
		ws.addFloat(e)
	} else {
		ws.addTile(e)
	}
	m.retarget(ws)
	return cmds
}

// Opens a workspace the way the app's workspace.new command does, nudging when none is registered
func (m *Model) NewWorkspace() tea.Cmd {
	if cmd := m.Run("workspace.new"); cmd != nil {
		return cmd
	}
	return m.nudge()
}

// Closes the workspace on the stage through the app's workspace.close command, else directly
func (m *Model) CloseWorkspace() tea.Cmd {
	for _, c := range m.Commands() {
		if c.ID == "workspace.close" {
			if !c.Ready() {
				return m.nudge()
			}
			m.remember(c.ID)
			return c.Run()
		}
	}
	return m.RemoveWorkspace(m.active)
}

// Advances every animation by dt seconds and reports whether anything is still moving
func (m *Model) Advance(dt float64) bool {
	moving := false
	for i, ws := range m.spaces {
		shown := i == m.active
		if shown || ws == m.leaving {
			if m.stepWorkspace(ws, dt, shown) {
				moving = true
			} else if ws == m.leaving {
				m.leaving = nil
			}
			continue
		}
		moving = m.animateCards(ws, dt) || moving
	}
	if m.overlay != nil {
		moving = m.step(m.overlay, dt, true, false) || moving
	}
	if m.parting != nil {
		moving = m.step(m.parting, dt, false, false) || moving
		if m.motion == 0 || (m.parting.open.Settled() && m.parting.open.Pos < 0.01) {
			m.parting = nil
		}
	}
	m.veil.Set(flag(m.overlay != nil))
	if m.motion == 0 {
		m.veil.Snap()
	} else {
		moving = m.veil.Step(dt/m.motion) || moving
	}
	moving = m.stepToasts(dt) || moving
	moving = m.stepBar(dt) || moving
	if m.hold > 0 {
		m.hold--
		moving = true
	}
	m.phase += dt * flowSpeed * m.motion
	m.tick++
	return moving
}

// Steps the slide and every card of a workspace, dropping cards whose pop out settled
func (m *Model) stepWorkspace(ws *Workspace, dt float64, shown bool) bool {
	moving := false
	if m.motion == 0 {
		ws.shift.Snap()
	} else {
		moving = ws.shift.Step(dt/m.motion) || moving
	}
	for _, e := range ws.entries() {
		focused := shown && ws.focus == e && m.overlay == nil
		hovered := shown && m.hover == e
		moving = m.step(e, dt, focused, hovered) || moving
		if e.closing && e.open.Settled() && e.open.Pos < 0.01 {
			m.drop(e)
		}
	}
	return moving
}

// Keeps the cards of a workspace off the stage in step so they are at rest when shown again
func (m *Model) animateCards(ws *Workspace, dt float64) bool {
	moving := false
	for _, e := range ws.entries() {
		moving = m.animate(e, dt) || moving
	}
	return moving
}

// Steps the card's own motion when it has any, repainting it when its view changed
func (m *Model) animate(e *entry, dt float64) bool {
	a, ok := e.card.(Animator)
	if !ok {
		return false
	}
	if m.motion == 0 {
		dt = 0
	} else {
		dt /= m.motion
	}
	if !a.Animate(dt) {
		return false
	}
	e.dirty = true
	return dt > 0
}

// Reports whether the entry is a tile covered by another card filling its stage
func hidden(e *entry) bool {
	return e.ws != nil && e.ws.zoom != nil && e.ws.zoom != e && !e.floating
}

func (m *Model) step(e *entry, dt float64, focused, hovered bool) bool {
	if m.motion == 0 {
		if !e.held {
			e.rect.Snap()
		}
		e.open.Snap()
		e.lit.Set(flag(focused))
		e.lit.Snap()
		e.hover.Set(flag(hovered))
		e.hover.Snap()
		e.pulse.Set(flag(e.urgency != Calm))
		e.pulse.Snap()
		e.hide.Set(flag(hidden(e)))
		e.hide.Snap()
		e.shake.Snap()
		e.decode = 1
		m.animate(e, dt)
		return false
	}
	dt /= m.motion
	moving := false
	if !e.held {
		moving = e.rect.Step(dt)
	}
	moving = e.open.Step(dt) || moving
	e.lit.Set(flag(focused))
	moving = e.lit.Step(dt) || moving
	e.hover.Set(flag(hovered))
	moving = e.hover.Step(dt) || moving
	e.pulse.Set(flag(e.urgency != Calm))
	moving = e.pulse.Step(dt) || moving
	e.hide.Set(flag(hidden(e)))
	moving = e.hide.Step(dt) || moving
	moving = e.shake.Step(dt) || moving
	if e.urgency != Calm {
		moving = true
	}
	if e.decode < 1 {
		e.decode = min(1, e.decode+dt/decodeTime)
		moving = true
	}
	if a, ok := e.card.(Animator); ok && a.Animate(dt) {
		e.dirty = true
		moving = true
	}
	return moving
}
