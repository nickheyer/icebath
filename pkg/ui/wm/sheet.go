package wm

import (
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Quiet time after a sheet or a guarded overlay comes into view before keys and clicks reach it
const SheetArmDelay = 600 * time.Millisecond

// Gap a burst of typing must leave before keys reach a sheet instead of its card
const sheetBurstGap = 300 * time.Millisecond

// Card that gives the sheets it holds a part of its content, such as the rows above its prompt, in content coordinates
type SheetHolder interface {
	SheetArea(w, h int) Box
}

// Card that sizes itself to the room it is given, the content columns and rows it takes within w by h
type Fitter interface {
	FitIn(w, h int) (int, int)
}

// Overlay that, like a sheet, holds its answer keys back for a moment after it appears and while a burst of typing goes on
type Guarded interface {
	Guarded() bool
}

// Overlay or sheet that carries out its close key itself, a question declining and staying until its asker drops it
type SelfClosing interface {
	ClosesItself() bool
}

func closesItself(c Card) bool {
	s, ok := c.(SelfClosing)
	return ok && s.ClosesItself()
}

// Frame tick that repaints a sheet once its keys arm
type sheetTickMsg struct{}

// Tick that repaints after the guard of a sheet may have lapsed
func sheetTick(after time.Duration) tea.Cmd {
	return tea.Tick(after, func(time.Time) tea.Msg { return sheetTickMsg{} })
}

// Shows the sheet inside the frame of the card that asked for it, queued behind the sheets that card already holds, the card glowing while it
// is not focused. A card that is not open shows the sheet as an overlay
func (m *Model) Attach(card, sheet Card) tea.Cmd {
	if card == nil || sheet == nil || m.sheetEntry(sheet) != nil {
		return nil
	}
	host := m.entryOf(card)
	if host == nil {
		return m.ShowOverlay(sheet)
	}
	prev := m.owner
	m.owner = host.owner
	s := m.newEntry(sheet, nil)
	m.owner = prev
	s.host = host
	host.sheets = append(host.sheets, s)
	cmds := []tea.Cmd{m.routed(s, sheet.Init())}
	if len(host.sheets) == 1 {
		cmds = append(cmds, m.showSheet(host))
	}
	if host != m.focused() {
		cmds = append(cmds, m.attention(host, Pulse))
	}
	return tea.Batch(append(cmds, m.wake())...)
}

// Brings the first sheet of a card into view, its guard starting now
func (m *Model) showSheet(host *entry) tea.Cmd {
	s := host.sheets[0]
	s.shownAt, s.lastKey = m.now(), time.Time{}
	s.dirty = true
	return sheetTick(SheetArmDelay)
}

// Takes the sheet out of the card it sits in, the next one waiting there coming into view
func (m *Model) Detach(sheet Card) tea.Cmd {
	s := m.sheetEntry(sheet)
	if s == nil {
		return nil
	}
	host := s.host
	i := slices.Index(host.sheets, s)
	host.sheets = slices.Delete(host.sheets, i, i+1)
	cmds := []tea.Cmd{m.send(s, FocusMsg{})}
	if i == 0 && len(host.sheets) > 0 {
		cmds = append(cmds, m.showSheet(host))
	}
	if len(host.sheets) == 0 && host.urgency == Pulse {
		host.urgency = Calm
	}
	host.dirty = true
	return tea.Batch(append(cmds, m.wake())...)
}

// Drops every sheet of a card that is closing, each told it lost its place
func (m *Model) dropSheets(host *entry) tea.Cmd {
	var cmds []tea.Cmd
	for _, s := range host.sheets {
		cmds = append(cmds, m.send(s, FocusMsg{}), m.send(s, ClosedMsg{}))
	}
	host.sheets = nil
	return tea.Batch(cmds...)
}

// Sheet the card shows now, nil when it shows none
func (m *Model) Sheet(card Card) Card {
	if e := m.entryOf(card); e != nil && len(e.sheets) > 0 {
		return e.sheets[0].card
	}
	return nil
}

// Whether the card waits as a sheet inside another card's frame
func (m *Model) Attached(sheet Card) bool { return m.sheetEntry(sheet) != nil }

// Entry of a sheet wherever it sits, nil when the card is no sheet
func (m *Model) sheetEntry(c Card) *entry {
	if c == nil {
		return nil
	}
	for _, ws := range m.spaces {
		for _, e := range ws.entries() {
			for _, s := range e.sheets {
				if s.card == c {
					return s
				}
			}
		}
	}
	return nil
}

// Every sheet of every workspace, those in view first and then the waiting ones, each workspace in order
func (m *Model) allSheets() []*entry {
	var shown, waiting []*entry
	for _, ws := range m.spaces {
		for _, e := range ws.entries() {
			for i, s := range e.sheets {
				if i == 0 {
					shown = append(shown, s)
				} else {
					waiting = append(waiting, s)
				}
			}
		}
	}
	return append(shown, waiting...)
}

// Sheet in view in the card, nil when it shows none
func showing(e *entry) *entry {
	if e == nil || len(e.sheets) == 0 {
		return nil
	}
	return e.sheets[0]
}

// Whether keys reach the sheet now: not right after it came into view, nor amid a burst of typing that went to its card
func (m *Model) armed(s *entry) bool {
	now := m.now()
	return now.Sub(s.shownAt) >= SheetArmDelay && now.Sub(s.lastKey) >= sheetBurstGap
}

// Whether the card of an overlay asks for the guard
func (m *Model) guards(e *entry) bool {
	g, ok := e.card.(Guarded)
	return ok && g.Guarded()
}

// Whether a guarded overlay holds its keys back now, every key held back extending the guard
func (m *Model) overlayHeld(e *entry) (bool, tea.Cmd) {
	if !m.guards(e) {
		return false, nil
	}
	if m.armed(e) {
		return false, nil
	}
	e.lastKey = m.now()
	return true, sheetTick(sheetBurstGap)
}

// Box of the sheet in view in a card, in the coordinates of the card's face: at the bottom of the part the card gives sheets, else of its content,
// as tall as the sheet asks
func (m *Model) sheetBox(e *entry, c Box) Box {
	area := c
	if h, ok := e.card.(SheetHolder); ok {
		a := h.SheetArea(c.W, c.H)
		if a.W >= minTitleW+4 && a.H >= 4 {
			area = Box{c.X + a.X, c.Y + a.Y, min(a.W, c.W-a.X), min(a.H, c.H-a.Y)}
		}
	}
	s := showing(e)
	room := max(area.W-4, 1)
	w, h := minSize(s.card)
	if f, ok := s.card.(Fitter); ok {
		w, h = f.FitIn(room, max(area.H-2, 1))
	}
	if w <= 0 || w > room {
		w = room
	}
	bw, bh := min(w+4, area.W), min(max(h, 1)+2, area.H)
	return Box{area.X + (area.W-bw)/2, area.Y + area.H - bh, bw, bh}
}

// Box of the sheet in view in a card on the stage, false when it shows none
func (m *Model) sheetScreen(e *entry) (Box, bool) {
	if showing(e) == nil {
		return Box{}, false
	}
	f := e.frame()
	c := m.contentRect(Box{0, 0, f.W, f.H})
	b := m.sheetBox(e, c)
	return Box{f.X + b.X, f.Y + b.Y, b.W, b.H}, b.W >= minTitleW && b.H >= 3
}

// Content box of a sheet frame
func sheetInner(b Box) Box { return Box{b.X + 2, b.Y + 1, b.W - 4, b.H - 2} }

// Paints the sheet in view inside the face of its card: a raised frame with its title and place in the queue, its content and along its bottom the
// keys it answers, or while it holds its keys back a line saying so
func (m *Model) paintSheet(face screen, e *entry, c Box, sx int, lit float64, focused bool) {
	s := showing(e)
	if s == nil {
		return
	}
	b := m.sheetBox(e, c)
	if b.W < minTitleW || b.H < 3 {
		return
	}
	t := m.theme
	bg := t.Surface
	if !t.Plain {
		bg = theme.Mix(t.Surface, t.Ice, 0.06)
	}
	fill := uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: bg}}
	face.FillArea(&fill, uv.Rect(b.X, b.Y, b.W, b.H))
	rim := theme.Mix(t.Steel, t.Pulse, 0.35+0.65*lit)
	if t.Plain {
		rim = t.Text
	}
	m.perimeter(b.X, b.Y, b.W, b.H, func(px, py int, ch string) {
		m.put(face, px, py, ch, uv.Style{Fg: rim, Bg: bg})
	})
	in := sheetInner(b)
	if in.W >= 1 && in.H >= 1 {
		buf := m.content(s, in.W, in.H, (1-lit)*contentDim)
		buf.Draw(face, uv.Rect(in.X, in.Y, in.W, in.H))
	}
	base := lipgloss.NewStyle().Background(bg)
	title := s.card.Title()
	if n := len(e.sheets); n > 1 {
		title += " · " + lang.T(txt.KitWmSheetOf, 1, n)
	}
	if !focused {
		title += " · " + lang.T(txt.KitWmSheetWaiting)
	}
	label := base.Render(" ") + base.Foreground(theme.Mix(t.TextDim, t.Pulse, 0.5+0.5*lit)).Bold(true).Render(t.Glyphs.Flag+" "+title) + base.Render(" ")
	if ansi.StringWidth(label) > b.W-4 {
		label = ansi.Truncate(label, b.W-5, t.Glyphs.Ellipsis) + base.Render(" ")
	}
	uv.NewStyledString(label).Draw(face, uv.Rect(b.X+2, b.Y, ansi.StringWidth(label), 1))
	s.hints = s.hints[:0]
	switch {
	case !focused:
	case !m.armed(s):
		text := base.Render(" ") + base.Foreground(t.TextDim).Italic(true).Render(lang.T(txt.KitWmSheetArming)) + base.Render(" ")
		uv.NewStyledString(text).Draw(face, uv.Rect(b.X+2, b.Y+b.H-1, ansi.StringWidth(text), 1))
	default:
		m.paintHints(face, s, b.X, b.Y+b.H-1, sx, b.W)
	}
}

// Cursor of the sheet in view of the focused card in screen space, nil while its keys are held back or it shows none
func (m *Model) sheetCursor(e *entry) (*tea.Cursor, bool) {
	s := showing(e)
	if s == nil {
		return nil, false
	}
	if !m.armed(s) {
		return nil, false
	}
	b, ok := m.sheetScreen(e)
	cz, has := s.card.(Cursorer)
	if !ok || !has {
		return nil, true
	}
	c := cz.Cursor()
	if c == nil {
		return nil, true
	}
	in := sheetInner(b)
	if c.X < 0 || c.Y < 0 || c.X >= in.W || c.Y >= in.H {
		return nil, true
	}
	out := *c
	out.X, out.Y = in.X+c.X, in.Y+c.Y+barHeight
	return &out, true
}

// Mouse press on the stage point of a card that shows a sheet there: a click on its content or on a key of its bottom edge goes to the sheet, the
// guard keeping a click as it appears from answering; reports whether the point lies on the sheet
func (m *Model) sheetPress(e *entry, msg tea.MouseClickMsg, sx, sy int) (tea.Cmd, bool) {
	b, ok := m.sheetScreen(e)
	if !ok || !b.contains(sx, sy) {
		return nil, false
	}
	s := showing(e)
	focus := m.focusEntry(e)
	if m.now().Sub(s.shownAt) < SheetArmDelay {
		return focus, true
	}
	if sy == b.Y+b.H-1 && msg.Button == tea.MouseLeft {
		if id := m.hintAt(s, sx); id != "" {
			return tea.Batch(focus, m.fireAt(s, id)), true
		}
	}
	in := sheetInner(b)
	if !in.contains(sx, sy) {
		return focus, true
	}
	mo := tea.Mouse(msg)
	mo.X, mo.Y = sx-in.X, sy-in.Y
	return tea.Batch(focus, m.send(s, tea.MouseClickMsg(mo))), true
}

// Wheel turned over a sheet, handed to it in its content coordinates; reports whether the point lies on the sheet
func (m *Model) sheetWheel(e *entry, msg tea.MouseWheelMsg, sx, sy int) (tea.Cmd, bool) {
	b, ok := m.sheetScreen(e)
	if !ok || !b.contains(sx, sy) {
		return nil, false
	}
	in := sheetInner(b)
	mo := tea.Mouse(msg)
	mo.X, mo.Y = sx-in.X, sy-in.Y
	return m.send(showing(e), tea.MouseWheelMsg(mo)), true
}

// Focuses the card a sheet waits in, the sheets in view first and the oldest first, then a card that glows for attention, skipping the focused card
func (m *Model) FocusAttention() tea.Cmd {
	focused := m.focused()
	var best *entry
	for _, s := range m.allSheets() {
		if s.host != focused && (best == nil || s.shownAt.Before(best.shownAt)) {
			best = s
		}
	}
	if best != nil {
		return m.focusEntry(best.host)
	}
	for _, ws := range m.spaces {
		for _, e := range ws.entries() {
			if e != focused && e.urgency != Calm {
				return m.focusEntry(e)
			}
		}
	}
	return m.nudge()
}

// Bar lanes and notice with every sheet waiting outside the focused card counted: a workspace holding one reads as blocked, and the notice names the
// card and the sheet with the number waiting behind it
func (m *Model) withSheets(st Status) Status {
	focused := m.focused()
	var waiting []*entry
	for _, s := range m.allSheets() {
		if s.host != focused {
			waiting = append(waiting, s)
		}
	}
	if len(waiting) == 0 {
		return st
	}
	lanes := make(map[*Workspace]Lane, len(st.Lanes))
	for ws, l := range st.Lanes {
		lanes[ws] = l
	}
	for _, s := range waiting {
		if l := lanes[s.host.ws]; l.Signal != Blocked {
			l.Signal, l.Detail = Blocked, lang.T(txt.KitWmSheetWaiting)
			lanes[s.host.ws] = l
		}
	}
	st.Lanes = lanes
	if st.Notice.Text == "" {
		first := waiting[0]
		st.Notice = Notice{Text: strings.TrimSpace(first.host.card.Title()) + ": " + first.card.Title(), Signal: Blocked, More: len(waiting) - 1, Action: "window.attention"}
	}
	return st
}

// Whether a card other than the focused one waits on the person, with a sheet or a glow
func (m *Model) needsYou() bool {
	focused := m.focused()
	for _, ws := range m.spaces {
		for _, e := range ws.entries() {
			if e != focused && (len(e.sheets) > 0 || e.urgency != Calm) {
				return true
			}
		}
	}
	return false
}
