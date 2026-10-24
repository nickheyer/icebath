package wm

import (
	tea "charm.land/bubbletea/v2"
)

// Part of a frame under the pointer
type region int

const (
	rNone region = iota
	rContent
	rTitle
	rLeft
	rRight
	rBottom
	rClose
)

// Column of the close button inside the top edge of a frame
func closeX(f Box) int { return f.X + f.W - 3 }

// Part of the frame box under the stage point
func regionOf(f Box, x, y int) region {
	switch {
	case !f.contains(x, y):
		return rNone
	case y == f.Y && x == closeX(f) && f.W >= minTitleW:
		return rClose
	case y == f.Y:
		return rTitle
	case y == f.Y+f.H-1:
		return rBottom
	case x == f.X:
		return rLeft
	case x == f.X+f.W-1:
		return rRight
	}
	return rContent
}

// Fires the action the keymap binds the key to in the contexts of the overlay or the focused card, else hands the key over as text
func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	m.sel = nil
	target := m.overlay
	if target == nil {
		target = m.focused()
	}
	if target == nil {
		if cmd, ok := m.dispatch(&entry{card: blank{}}, msg); ok {
			return cmd
		}
		return nil
	}
	if cmd, ok := m.dispatch(target, msg); ok {
		return cmd
	}
	return m.send(target, msg)
}

// Card standing in for an empty stage so global binds still fire
type blank struct{}

func (blank) Title() string                  { return "" }
func (blank) Init() tea.Cmd                  { return nil }
func (blank) Update(tea.Msg) (Card, tea.Cmd) { return blank{}, nil }
func (blank) View(int, int) string           { return "" }

// Routes the mouse to the bar, the overlay, or the card under the pointer
func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	mo := msg.Mouse()
	sx, sy := mo.X, mo.Y-barHeight
	if m.overlay != nil {
		return m.overlayMouse(msg, sx, sy)
	}
	switch msg := msg.(type) {
	case tea.MouseMotionMsg:
		return m.motion_(mo, sx, sy)
	case tea.MouseClickMsg:
		return m.press(msg, sx, sy)
	case tea.MouseReleaseMsg:
		return m.release(msg, sx, sy)
	case tea.MouseWheelMsg:
		if mo.Y < barHeight {
			switch msg.Button {
			case tea.MouseWheelDown, tea.MouseWheelRight:
				return m.Switch(min(m.active+1, len(m.spaces)-1))
			case tea.MouseWheelUp, tea.MouseWheelLeft:
				return m.Switch(max(m.active-1, 0))
			}
			return nil
		}
		if ws := m.Active(); ws != nil {
			if e := ws.at(sx, sy); e != nil {
				return m.forward(e, msg)
			}
		}
	}
	return nil
}

// Routes mouse input to the overlay only, a click outside it dismissing it
func (m *Model) overlayMouse(msg tea.MouseMsg, sx, sy int) tea.Cmd {
	f := m.overlay.frame()
	if !f.contains(sx, sy) {
		if _, click := msg.(tea.MouseClickMsg); click {
			return m.Dismiss()
		}
		return nil
	}
	if click, ok := msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft && regionOf(f, sx, sy) == rBottom {
		if id := m.hintAt(m.overlay, sx); id != "" {
			return m.fireAt(m.overlay, id)
		}
	}
	return m.forward(m.overlay, msg)
}

// Delivers a mouse message to the card in content coordinates
func (m *Model) forward(e *entry, msg tea.MouseMsg) tea.Cmd {
	c := m.contentRect(e.frame())
	mo := msg.Mouse()
	mo.X -= c.X
	mo.Y -= c.Y + barHeight
	return m.send(e, relocate(msg, mo))
}

// Mouse message of the same kind at another position
func relocate(msg tea.MouseMsg, mo tea.Mouse) tea.Msg {
	switch msg.(type) {
	case tea.MouseClickMsg:
		return tea.MouseClickMsg(mo)
	case tea.MouseReleaseMsg:
		return tea.MouseReleaseMsg(mo)
	case tea.MouseWheelMsg:
		return tea.MouseWheelMsg(mo)
	}
	return tea.MouseMotionMsg(mo)
}

// Tracks the pointer for drags, resizes, text sweeps and hover
func (m *Model) motion_(mo tea.Mouse, sx, sy int) tea.Cmd {
	if m.resizer != nil {
		if mo.Button != tea.MouseLeft {
			m.resizer = nil
			return nil
		}
		m.applyResize(sx, sy)
		return m.wake()
	}
	if m.grab != nil {
		if mo.Button == tea.MouseLeft {
			return m.drag(sx, sy)
		}
		m.letGo()
	}
	if m.sel != nil && mo.Button == tea.MouseLeft {
		if m.extendSelection(sx, sy) {
			return m.wake()
		}
		return nil
	}
	hot := -1
	menu := false
	if mo.Y < barHeight {
		hot = m.chipAt(mo.X)
		menu = m.onMenu(mo.X)
	}
	var hit *entry
	reg := rNone
	if ws := m.Active(); ws != nil && mo.Y >= barHeight {
		if hit = ws.at(sx, sy); hit != nil {
			reg = regionOf(hit.frame(), sx, sy)
		}
	}
	grip := false
	if hit != nil && !hit.closing {
		switch reg {
		case rTitle:
			grip = true
		case rLeft, rRight:
			grip = m.resizerFor(m.Active(), hit, reg) != nil
		case rBottom:
			grip = m.hintAt(hit, sx) != "" || m.resizerFor(m.Active(), hit, reg) != nil
		case rClose:
			grip = closable(hit.card)
		}
	}
	chipHot := -1
	if mo.Y < barHeight {
		chipHot = m.chipPartAt(mo.X)
	}
	hintHot := ""
	if hit != nil && reg == rBottom {
		hintHot = m.hintAt(hit, sx)
	}
	slid := grip && (reg == rLeft || reg == rRight || reg == rBottom) && (sx != m.hoverX || sy != m.hoverY)
	m.hoverX, m.hoverY = sx, sy
	if hit == m.hover && reg == m.hoverEdge && hot == m.barHot && menu == m.menuHot && grip == m.hoverGrip && chipHot == m.chipHot && hintHot == m.hintHot && !slid {
		return nil
	}
	m.hover, m.hoverEdge, m.barHot, m.menuHot, m.hoverGrip, m.chipHot, m.hintHot = hit, reg, hot, menu, grip, chipHot, hintHot
	return m.wake()
}

// Focuses the pressed card, starts title drags, edge resizes and text sweeps, and closes on middle click
func (m *Model) press(msg tea.MouseClickMsg, sx, sy int) tea.Cmd {
	mo := tea.Mouse(msg)
	m.letGo()
	m.sel = nil
	if mo.Y < barHeight {
		if mo.Button != tea.MouseLeft {
			return nil
		}
		if m.onMenu(mo.X) {
			return m.OpenPalette()
		}
		switch m.chipPartAt(mo.X) {
		case chipNew:
			return m.NewWorkspace()
		case chipClose:
			return m.CloseWorkspace()
		}
		if i := m.chipAt(mo.X); i >= 0 {
			return m.Switch(i)
		}
		return nil
	}
	ws := m.Active()
	if ws == nil {
		return nil
	}
	e := ws.at(sx, sy)
	if e == nil {
		return nil
	}
	f := e.frame()
	reg := regionOf(f, sx, sy)
	if reg == rBottom && mo.Button == tea.MouseLeft {
		if id := m.hintAt(e, sx); id != "" {
			return tea.Batch(m.focusEntry(e), m.fireAt(e, id))
		}
	}
	if mo.Button == tea.MouseMiddle || (reg == rClose && mo.Button == tea.MouseLeft) {
		if !closable(e.card) {
			return m.nudge()
		}
		return m.closeEntry(e)
	}
	var cmds []tea.Cmd
	if reg == rContent {
		cmds = append(cmds, m.forward(e, msg))
	}
	if mo.Button != tea.MouseLeft {
		return tea.Batch(append(cmds, m.focusEntry(e))...)
	}
	now := m.now()
	double := e.id == m.clickID && now.Sub(m.lastClick) < doubleClick
	m.lastClick, m.clickID = now, e.id
	if double && reg == rTitle {
		m.lastClick = m.lastClick.Add(-doubleClick)
		return tea.Batch(append(cmds, m.setZoom(e, ws.zoom != e))...)
	}
	switch reg {
	case rTitle:
		m.grab = &grab{e: e, dx: sx - f.X, dy: sy - f.Y, x: sx, y: sy}
	case rLeft, rRight, rBottom:
		m.resizer = m.resizerFor(ws, e, reg)
	case rContent:
		m.beginSelection(e, sx, sy)
	}
	return tea.Batch(append(cmds, m.focusEntry(e))...)
}

// Resize a drag on the edge of the entry performs, nil when that edge is fixed
func (m *Model) resizerFor(ws *Workspace, e *entry, reg region) *resizer {
	if e.floating {
		return &resizer{e: e, edge: reg}
	}
	if len(ws.tiles) < 2 || ws.zoom != nil {
		return nil
	}
	if ws.mode == Dwindle {
		if s := ws.splitFor(e, reg); s != nil {
			return &resizer{e: e, edge: reg, split: s}
		}
		return nil
	}
	master := ws.tiles[0]
	wide := master.box.W < m.width
	switch {
	case e == master && wide && reg == rRight, e == master && !wide && reg == rBottom:
		return &resizer{e: e, edge: reg}
	case e != master && wide && reg == rLeft && ws.neighbor(e, Left) == master:
		return &resizer{e: e, edge: reg}
	case e != master && reg == rBottom:
		if below := ws.neighbor(e, Down); below != nil && below != master {
			return &resizer{e: e, edge: reg, other: below}
		}
	}
	return nil
}

// Moves the dragged edge to the pointer, snapping the layout so it tracks the hand
func (m *Model) applyResize(sx, sy int) {
	r := m.resizer
	ws := r.e.ws
	sw, sh := m.width, m.stageHeight()
	switch {
	case r.e.floating:
		b := r.e.fbox
		switch r.edge {
		case rLeft:
			right := b.X + b.W
			b.X = min(max(sx, 0), right-12)
			b.W = right - b.X
		case rRight:
			b.W = max(sx-b.X+1, 12)
		case rBottom:
			b.H = max(sy-b.Y+1, 4)
		}
		r.e.fbox = clampBox(b, sw, sh)
	case r.split != nil:
		box := r.split.box
		if r.split.vertical {
			r.split.ratio = clampSplit(float64(sx-box.X+1) / float64(max(box.W, 1)))
		} else {
			r.split.ratio = clampSplit(float64(sy-box.Y+1) / float64(max(box.H, 1)))
		}
	case r.other != nil:
		a, b := r.e, r.other
		top, bottom := a.box.Y, b.box.Y+b.box.H
		ha := min(max(sy-top+1, minRows), bottom-top-m.gap-minRows)
		hb := bottom - top - m.gap - ha
		total := a.weight + b.weight
		a.weight = total * float64(ha) / float64(ha+hb)
		b.weight = total - a.weight
	default:
		st := m.stage()
		if ws.tiles[0].box.W < st.W {
			ws.ratio = clampRatio(float64(sx-st.X) / float64(max(st.W, 1)))
		} else {
			ws.ratio = clampRatio(float64(sy-st.Y) / float64(max(st.H, 1)))
		}
	}
	m.retarget(ws)
	for _, e := range ws.entries() {
		if !e.closing {
			e.rect.Snap()
		}
	}
}

// Carries the grabbed card with the pointer, trading tiles live as it crosses their slots
func (m *Model) drag(sx, sy int) tea.Cmd {
	g := m.grab
	if !g.moved {
		if sx == g.x && sy == g.y {
			return nil
		}
		g.moved, g.e.held = true, true
	}
	ws := g.e.ws
	g.e.rect.X.Pos, g.e.rect.X.Vel = float64(sx-g.dx), 0
	g.e.rect.Y.Pos, g.e.rect.Y.Vel = float64(sy-g.dy), 0
	if g.e.floating {
		g.e.fbox.X, g.e.fbox.Y = sx-g.dx, sy-g.dy
		g.e.fbox = clampBox(g.e.fbox, m.width, m.stageHeight())
		g.e.box = g.e.fbox
		g.e.rect.Set(float64(g.e.box.X), float64(g.e.box.Y), float64(g.e.box.W), float64(g.e.box.H))
		return m.wake()
	}
	if ws.zoom != nil {
		return m.wake()
	}
	if slot := ws.slotAt(sx, sy); slot >= 0 && ws.tiles[slot] != g.e && !ws.tiles[slot].closing {
		ws.swap(g.e, ws.tiles[slot])
		m.retarget(ws)
	}
	return m.wake()
}

// Ends drags, resizes and sweeps, letting a carried card spring into its slot
func (m *Model) release(msg tea.MouseReleaseMsg, sx, sy int) tea.Cmd {
	if m.resizer != nil {
		m.resizer = nil
		return m.wake()
	}
	if m.sel != nil {
		cmd := m.endSelection()
		if ws := m.Active(); ws != nil {
			if e := ws.at(sx, sy); e != nil && regionOf(e.frame(), sx, sy) == rContent {
				return tea.Batch(cmd, m.forward(e, msg))
			}
		}
		return cmd
	}
	if g := m.grab; g != nil {
		moved := g.moved
		m.letGo()
		if moved {
			m.hover = nil
			m.retarget(g.e.ws)
			return m.wake()
		}
	}
	if ws := m.Active(); ws != nil {
		if e := ws.at(sx, sy); e != nil && regionOf(e.frame(), sx, sy) == rContent {
			return m.forward(e, msg)
		}
	}
	return nil
}

// Shows a card above everything with first claim on input, replacing any overlay up
func (m *Model) ShowOverlay(c Card) tea.Cmd {
	if c == nil {
		return nil
	}
	var cmds []tea.Cmd
	if m.overlay != nil {
		cmds = append(cmds, m.send(m.overlay, FocusMsg{}))
		m.part(m.overlay)
	}
	m.letGo()
	m.sel = nil
	e := m.newEntry(c, nil)
	m.overlay = e
	m.veil.Set(1)
	if m.motion == 0 {
		m.veil.Snap()
	}
	m.placeOverlay()
	cmds = append(cmds, c.Init(), m.send(e, FocusMsg{Focused: true}), m.wake())
	return tea.Batch(cmds...)
}

// Drops the overlay and hands input back to the focused card, the sheet fading out behind the veil
func (m *Model) Dismiss() tea.Cmd {
	if m.overlay == nil {
		return nil
	}
	e := m.overlay
	m.overlay = nil
	m.part(e)
	m.veil.Set(0)
	if m.motion == 0 {
		m.veil.Snap()
	}
	return tea.Batch(m.send(e, FocusMsg{}), m.wake())
}

// Keeps a dropped overlay on screen while it shrinks away, no longer taking input
func (m *Model) part(e *entry) {
	if m.motion == 0 {
		m.parting = nil
		return
	}
	e.closing = true
	e.open.Set(0)
	m.aim(e)
	m.parting = e
}

// Card shown above everything, nil when none is
func (m *Model) Overlay() Card {
	if m.overlay == nil {
		return nil
	}
	return m.overlay.card
}

// Rows a palette sits below the top of the stage, clear of the title row of the cards under it
const paletteDrop = 2

// Sizes the overlay from its card and centers it, palettes sitting near the top
func (m *Model) placeOverlay() {
	e := m.overlay
	if e == nil || m.width == 0 {
		return
	}
	sw, sh := m.width, m.stageHeight()
	w, h := minSize(e.card)
	y := -1
	if p, ok := e.card.(*Palette); ok {
		w = min(max(sw*3/5, 44), sw) - 2 - 2*m.padX
		h = min(max(p.Rows(), 1), max(sh-2-paletteDrop-2*m.padY, 1), 14)
		y = paletteDrop
	}
	if w <= 0 {
		w = sw * 7 / 10
	}
	if h <= 0 {
		h = sh * 7 / 10
	}
	w, h = min(max(w+2+2*m.padX, 12), sw), min(max(h+2+2*m.padY, 4), sh)
	if y < 0 {
		y = (sh - h) / 2
	}
	e.box = clampBox(Box{(sw - w) / 2, y, w, h}, sw, sh)
	m.aim(e)
}

// Action of the legend entry painted under column x of the bottom edge of the entry, empty off any entry
func (m *Model) hintAt(e *entry, x int) string {
	for _, h := range e.hints {
		if x >= h.x0 && x < h.x1 {
			return h.id
		}
	}
	return ""
}

// Runs the command with the id, or fires the action at the entry
func (m *Model) fireAt(e *entry, id string) tea.Cmd {
	if c, ok := m.command(id); ok {
		if !c.Ready() {
			return m.nudge()
		}
		m.remember(id)
		return c.Run()
	}
	return m.send(e, ActionMsg{ID: id})
}
