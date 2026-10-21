package deck

import (
	"slices"
	"strconv"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Reports whether the card claims the key ahead of the deck bindings
func claims(c Card, k string) bool {
	cl, ok := c.(Claimer)
	if !ok {
		return false
	}
	for _, want := range cl.Claims() {
		if want == k {
			return true
		}
	}
	return false
}

func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	m.sel = nil
	if msg.String() == "ctrl+c" {
		return tea.Quit
	}
	k := m.Keys
	if m.overlay != nil {
		if msg.String() == "esc" && !m.Capturing() && !claims(m.overlay.card, "esc") {
			return m.Dismiss()
		}
		return m.send(m.overlay, msg)
	}
	if len(m.cards) == 0 {
		switch {
		case key.Matches(msg, k.Quit):
			return tea.Quit
		case key.Matches(msg, k.Help):
			return m.toggleHints()
		}
		return nil
	}
	e := m.cards[m.focus]
	if claims(e.card, msg.String()) {
		return m.send(e, msg)
	}
	if m.Capturing() {
		switch msg.String() {
		case "tab":
			return m.cycle(1)
		case "shift+tab":
			return m.cycle(-1)
		}
		return m.send(e, msg)
	}
	switch {
	case key.Matches(msg, k.Quit):
		return tea.Quit
	case key.Matches(msg, k.Next):
		return m.cycle(1)
	case key.Matches(msg, k.Prev):
		return m.cycle(-1)
	case key.Matches(msg, k.ToMain):
		return m.toMain()
	case key.Matches(msg, k.ToStack):
		return m.toStack()
	case key.Matches(msg, k.Enter):
		return m.dive()
	case key.Matches(msg, k.Back):
		return m.back()
	case key.Matches(msg, k.Grow):
		return m.resize(ratioStep)
	case key.Matches(msg, k.Shrink):
		return m.resize(-ratioStep)
	case key.Matches(msg, k.MoveDown):
		return m.shift(1)
	case key.Matches(msg, k.MoveUp):
		return m.shift(-1)
	case key.Matches(msg, k.Close):
		if isPinned(e.card) {
			return m.nudge()
		}
		return m.Remove(m.focus)
	case key.Matches(msg, k.Jump):
		return m.jump(msg)
	case key.Matches(msg, k.Timeline):
		return m.toggleTimeline()
	case key.Matches(msg, k.Help):
		return m.toggleHints()
	}
	return m.send(e, msg)
}

func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if m.overlay != nil {
		return m.overlayMouse(msg)
	}
	switch msg := msg.(type) {
	case tea.MouseMotionMsg:
		return m.motion_(tea.Mouse(msg))
	case tea.MouseClickMsg:
		return m.press(msg)
	case tea.MouseReleaseMsg:
		return m.release(msg)
	case tea.MouseWheelMsg:
		if m.mode == Timeline && m.hit(msg.X, msg.Y) < 0 {
			return m.scrub(msg)
		}
		if hit := m.hit(msg.X, msg.Y); hit >= 0 {
			if m.mode == Timeline && hit != m.focus {
				return m.scrub(msg)
			}
			return m.forwardMouse(hit, msg)
		}
	}
	return nil
}

// Routes mouse input to the overlay only, everything outside it ignored
func (m *Model) overlayMouse(msg tea.MouseMsg) tea.Cmd {
	mo := msg.Mouse()
	if !m.inOverlay(mo.X, mo.Y) {
		if _, click := msg.(tea.MouseClickMsg); click && m.hintAt(mo.X, mo.Y) >= 0 && mo.Button == tea.MouseLeft {
			return m.zones[m.hintAt(mo.X, mo.Y)].act()
		}
		return nil
	}
	x, y, _, _ := m.overlay.rect.Cells()
	mo.X -= x + 1
	mo.Y -= y + 1
	return m.send(m.overlay, relocate(msg, mo))
}

// Moves focus along the timeline by wheel direction
func (m *Model) scrub(msg tea.MouseWheelMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseWheelDown, tea.MouseWheelRight:
		return m.Focus(min(m.focus+1, len(m.cards)-1))
	case tea.MouseWheelUp, tea.MouseWheelLeft:
		return m.Focus(max(m.focus-1, 0))
	}
	return nil
}

// Tracks the pointer for hover, splitter drags, card drags, timeline scrubs and text sweeps
func (m *Model) motion_(mo tea.Mouse) tea.Cmd {
	if m.dragging {
		m.SetRatio(float64(mo.X) / float64(max(m.width, 1)))
		m.snap()
		return m.wake()
	}
	if m.scrubbing {
		if mo.Button != tea.MouseLeft {
			m.scrubbing = false
			return nil
		}
		steps := (mo.X - m.scrubX) / timelineStep
		if steps == 0 {
			return nil
		}
		m.scrubX += steps * timelineStep
		return m.Focus(min(max(m.focus-steps, 0), len(m.cards)-1))
	}
	if m.sel != nil && mo.Button == tea.MouseLeft {
		if m.extendSelection(mo.X, mo.Y) {
			return m.wake()
		}
		return nil
	}
	if m.grab != nil {
		if mo.Button == tea.MouseLeft {
			return m.drag(mo)
		}
		m.letGo()
	}
	hot := m.onGutter(mo.X, mo.Y)
	hit := -1
	if !hot {
		hit = m.hit(mo.X, mo.Y)
	}
	hint := m.hintAt(mo.X, mo.Y)
	if hit == m.hover && hot == m.gutterHot && hint == m.hintHot {
		return nil
	}
	m.hover, m.gutterHot, m.hintHot = hit, hot, hint
	return m.wake()
}

// Focuses the pressed card, starts splitter, frame or sweep drags, runs hint clicks, and closes on middle click
func (m *Model) press(msg tea.MouseClickMsg) tea.Cmd {
	mo := tea.Mouse(msg)
	m.letGo()
	m.sel = nil
	if z := m.hintAt(mo.X, mo.Y); z >= 0 {
		if mo.Button == tea.MouseLeft {
			return m.zones[z].act()
		}
		return nil
	}
	if mo.Button == tea.MouseLeft && m.onGutter(mo.X, mo.Y) {
		m.dragging = true
		return m.wake()
	}
	hit := m.hit(mo.X, mo.Y)
	if hit < 0 {
		if m.mode == Timeline && mo.Button == tea.MouseLeft && mo.Y < m.stageHeight() {
			m.scrubbing, m.scrubX = true, mo.X
		}
		return nil
	}
	if mo.Button == tea.MouseMiddle {
		if isPinned(m.cards[hit].card) {
			return m.nudge()
		}
		return m.Remove(hit)
	}
	var cmds []tea.Cmd
	content := m.inContent(hit, mo.X, mo.Y)
	if content {
		cmds = append(cmds, m.forwardMouse(hit, msg))
	}
	if mo.Button != tea.MouseLeft {
		return tea.Batch(cmds...)
	}
	e := m.cards[hit]
	now := m.now()
	double := hit == m.focus && e.id == m.clickID && now.Sub(m.lastClick) < doubleClick
	m.lastClick, m.clickID = now, e.id
	if double {
		m.lastClick = time.Time{}
		return tea.Batch(append(cmds, m.dive())...)
	}
	switch {
	case content && hit == m.focus:
		m.beginSelection(hit, mo.X, mo.Y)
	case !content && m.mode == Split && len(m.cards) > 1:
		x, y, _, _ := e.rect.Cells()
		m.grab = &grab{e: e, dx: mo.X - x, dy: mo.Y - y, x: mo.X, y: mo.Y}
	case !content && m.mode == Timeline && hit != m.focus:
	}
	return tea.Batch(append(cmds, m.Focus(hit))...)
}

// Ends splitter, card and sweep drags, trading a card dropped on the other column with the main slot
func (m *Model) release(msg tea.MouseReleaseMsg) tea.Cmd {
	if m.dragging {
		m.dragging = false
		return m.wake()
	}
	if m.scrubbing {
		m.scrubbing = false
		return nil
	}
	if m.sel != nil {
		cmd := m.endSelection()
		if hit := m.hit(msg.X, msg.Y); hit >= 0 && m.inContent(hit, msg.X, msg.Y) {
			return tea.Batch(cmd, m.forwardMouse(hit, msg))
		}
		return cmd
	}
	if g := m.grab; g != nil {
		drop, moved := m.drop, g.moved
		from := slices.Index(m.cards, g.e)
		m.letGo()
		if moved {
			m.hover = -1
			if drop >= 0 && from >= 0 {
				stack := from
				if from == 0 {
					stack = drop
				}
				return m.Promote(stack)
			}
			return m.wake()
		}
	}
	if hit := m.hit(msg.X, msg.Y); hit >= 0 && m.inContent(hit, msg.X, msg.Y) {
		return m.forwardMouse(hit, msg)
	}
	return nil
}

// Carries the grabbed card with the pointer, reordering the stack or marking a drop onto the other column
func (m *Model) drag(mo tea.Mouse) tea.Cmd {
	g := m.grab
	if !g.moved {
		if mo.X == g.x && mo.Y == g.y {
			return nil
		}
		g.moved, g.e.held = true, true
	}
	g.e.rect.X.Pos, g.e.rect.X.Vel = float64(mo.X-g.dx), 0
	g.e.rect.Y.Pos, g.e.rect.Y.Vel = float64(mo.Y-g.dy), 0
	from := slices.Index(m.cards, g.e)
	slot := m.slotAt(mo.X, mo.Y)
	m.drop, m.hover = -1, -1
	switch {
	case from < 0 || slot < 0 || slot == from:
	case from > 0 && slot > 0:
		if !isPinned(m.cards[slot].card) {
			m.swap(from, slot)
		}
	default:
		if !isPinned(g.e.card) && !isPinned(m.cards[slot].card) {
			m.drop, m.hover = slot, slot
		}
	}
	return m.wake()
}

// Moves focus d steps through the deck order, wrapping at both ends
func (m *Model) cycle(d int) tea.Cmd {
	n := len(m.cards)
	if n < 2 {
		return m.nudge()
	}
	return m.Focus(((m.focus+d)%n + n) % n)
}

// Focuses the main card, or steps left along the timeline, or nudges when already there
func (m *Model) toMain() tea.Cmd {
	if m.mode == Timeline {
		if m.focus == 0 {
			return m.nudge()
		}
		return m.Focus(m.focus - 1)
	}
	if m.focus == 0 {
		return m.nudge()
	}
	return m.Focus(0)
}

// Focuses the remembered stack card, or steps right along the timeline, or nudges when focus is already in the stack
func (m *Model) toStack() tea.Cmd {
	if m.mode == Timeline {
		if m.focus >= len(m.cards)-1 {
			return m.nudge()
		}
		return m.Focus(m.focus + 1)
	}
	if m.focus != 0 || len(m.cards) < 2 {
		return m.nudge()
	}
	return m.Focus(min(max(m.stackMemo, 1), len(m.cards)-1))
}

// Promotes the focused stack card or opens the focused main card alone
func (m *Model) dive() tea.Cmd {
	if m.mode == Solo {
		return m.nudge()
	}
	if m.mode == Timeline {
		return m.SetMode(Solo)
	}
	return m.Promote(m.focus)
}

// Leaves solo or timeline mode, or nudges when the deck is already split
func (m *Model) back() tea.Cmd {
	if m.mode == Split {
		return m.nudge()
	}
	return m.SetMode(Split)
}

// Switches the timeline fan on or off
func (m *Model) toggleTimeline() tea.Cmd {
	if m.mode == Timeline {
		return m.SetMode(Split)
	}
	return m.SetMode(Timeline)
}

// Focuses the card whose on screen ordinal matches the pressed digit
func (m *Model) jump(msg tea.KeyPressMsg) tea.Cmd {
	s := msg.Text
	if s == "" {
		s = string(msg.Code)
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > len(m.cards) || n-1 == m.focus {
		return m.nudge()
	}
	return m.Focus(n - 1)
}

func (m *Model) resize(d float64) tea.Cmd {
	if m.mode != Split || len(m.cards) < 2 {
		return m.nudge()
	}
	if cmd := m.SetRatio(m.ratio + d); cmd != nil {
		return cmd
	}
	return m.nudge()
}

// Swaps the focused stack card with its neighbour d steps away
func (m *Model) shift(d int) tea.Cmd {
	j := m.focus + d
	if m.focus == 0 || j < 1 || j >= len(m.cards) {
		return m.nudge()
	}
	if isPinned(m.cards[m.focus].card) || isPinned(m.cards[j].card) {
		return m.nudge()
	}
	m.swap(m.focus, j)
	m.hover = -1
	return m.wake()
}

// Shakes the focused card sideways to show that a key had nothing to do
func (m *Model) nudge() tea.Cmd {
	if len(m.cards) == 0 || m.motion == 0 {
		return nil
	}
	m.cards[m.focus].rect.X.Kick(nudgeImpulse * m.motion)
	return m.wake()
}

// Index of the topmost card under the point, or -1
func (m *Model) hit(x, y int) int {
	best, bestZ := -1, -1
	n := len(m.cards)
	for i, e := range m.cards {
		cx, cy, cw, ch := e.rect.Cells()
		if x < cx || y < cy || x >= cx+cw || y >= cy+ch {
			continue
		}
		if z := m.z(e, i, n); z > bestZ {
			best, bestZ = i, z
		}
	}
	return best
}

// Index of the resting box under the point, or -1
func (m *Model) slotAt(x, y int) int {
	for i, b := range m.boxes {
		if x >= b.x && x < b.x+b.w && y >= b.y && y < b.y+b.h {
			return i
		}
	}
	return -1
}

// Reports whether the point lies inside card i's frame rather than on it
func (m *Model) inContent(i, x, y int) bool {
	cx, cy, cw, ch := m.cards[i].rect.Cells()
	return cw >= 3 && ch >= 3 && x > cx && x < cx+cw-1 && y > cy && y < cy+ch-1
}

// Stacking rank with cards in flight raised above everything at rest
func (m *Model) z(e *entry, i, n int) int {
	z := depth(i, n, m.focus)
	if m.mode == Timeline {
		z = timelineDepth(i, n, m.focus)
	}
	if e.lift.Pos > 0.3 {
		z += n + 4
	}
	return z
}

// Reports whether the point is on the splitter or the border column either side of it
func (m *Model) onGutter(x, y int) bool {
	if m.mode != Split || len(m.cards) < 2 || len(m.boxes) == 0 || m.width < narrowWidth {
		return false
	}
	gx := m.boxes[0].x + m.boxes[0].w
	return x >= gx-1 && x <= gx+1 && y >= 0 && y < m.stageHeight()
}

// Forwards a mouse event to card i with coordinates relative to its content origin
func (m *Model) forwardMouse(i int, msg tea.MouseMsg) tea.Cmd {
	e := m.cards[i]
	x, y, _, _ := e.rect.Cells()
	mo := msg.Mouse()
	mo.X -= x + 1
	mo.Y -= y + 1
	return m.send(e, relocate(msg, mo))
}

// Same kind of mouse event carrying new coordinates
func relocate(msg tea.MouseMsg, mo tea.Mouse) tea.Msg {
	switch msg.(type) {
	case tea.MouseClickMsg:
		return tea.MouseClickMsg(mo)
	case tea.MouseReleaseMsg:
		return tea.MouseReleaseMsg(mo)
	case tea.MouseWheelMsg:
		return tea.MouseWheelMsg(mo)
	case tea.MouseMotionMsg:
		return tea.MouseMotionMsg(mo)
	}
	return msg
}
