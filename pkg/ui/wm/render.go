package wm

import (
	"image/color"
	"math"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

const (
	minContentW = 4
	minContentH = 3
	minTitleW   = 8
)

// Padding a frame of that size affords, dropped on an axis where it would leave too little content
func (m *Model) inset(w, h int) (px, py int) {
	px, py = m.padX, m.padY
	if w-2-2*px < minContentW {
		px = 0
	}
	if h-2-2*py < minContentH {
		py = 0
	}
	return px, py
}

// Content box inside the border and padding of a frame, in the frame's coordinates
func (m *Model) contentRect(f Box) Box {
	px, py := m.inset(f.W, f.H)
	return Box{f.X + 1 + px, f.Y + 1 + py, f.W - 2 - 2*px, f.H - 2 - 2*py}
}

// Composites the bar, the stage, the veil, the overlay and the toasts into a full screen view with mouse tracking
func (m *Model) View() tea.View {
	buf := m.frame()
	buf.Fill(&uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: m.theme.Bg}})
	m.paintBar(buf)
	if m.leaving != nil {
		m.paintWorkspace(m.leaving, int(math.Round(m.leaving.shift.Pos)), false)
	}
	if ws := m.Active(); ws != nil {
		m.paintWorkspace(ws, int(math.Round(ws.shift.Pos)), true)
	}
	if m.veil.Pos > 0.005 {
		m.paintBackdrop(m.veil.Pos)
	}
	if m.parting != nil {
		m.paintBody(m.parting, 0, false, 0)
	}
	if m.overlay != nil {
		m.placeOverlay()
		m.paintBody(m.overlay, 0, true, 0)
	}
	m.paintToasts()
	m.paintSelection(buf)
	v := tea.NewView(buf.Render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeAllMotion
	v.BackgroundColor = m.theme.Bg
	v.WindowTitle = m.title
	v.Cursor = m.cursor()
	v.ReportFocus = true
	v.KeyboardEnhancements.ReportEventTypes = true
	v.ProgramStatus = m.status
	v.ProgressBar = m.progress
	return v
}

// Writes a cell onto the stage blended over what is there by alpha, text beneath fading out under a blank
func (m *Model) ink(x, y int, c uv.Cell, alpha float64) {
	if x < 0 || y < barHeight || x >= m.width || y >= m.height {
		return
	}
	if alpha >= 1 || m.theme.Plain {
		m.buf.SetCell(x, y, &c)
		return
	}
	t := m.theme
	old := m.buf.CellAt(x, y)
	oldBg, oldFg, keep := color.Color(t.Bg), color.Color(t.Text), false
	if old != nil {
		oldBg, oldFg = or(old.Style.Bg, t.Bg), or(old.Style.Fg, t.Text)
		keep = old.Width > 0 && old.Content != " " && old.Content != ""
	}
	bg := theme.Mix(oldBg, or(c.Style.Bg, t.Bg), alpha)
	if c.Content == " " || c.Content == "" {
		if keep {
			out := *old
			out.Style.Bg, out.Style.Fg = bg, theme.Mix(oldFg, bg, alpha)
			m.buf.SetCell(x, y, &out)
			return
		}
		m.buf.SetCell(x, y, &uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: bg}})
		return
	}
	c.Style.Bg, c.Style.Fg = bg, theme.Mix(bg, or(c.Style.Fg, t.Text), alpha)
	m.buf.SetCell(x, y, &c)
}

// Scratch buffer at least w by h filled with the surface, where one card is composed before it is stamped
func (m *Model) face(w, h int) screen {
	if m.card.Buffer == nil || m.card.Width() < w || m.card.Height() < h {
		cw, ch := w, h
		if m.card.Buffer != nil {
			cw, ch = max(cw, m.card.Width()), max(ch, m.card.Height())
		}
		m.card = screen{uv.NewBuffer(cw, ch)}
	}
	m.card.FillArea(&uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: m.theme.Surface}}, uv.Rect(0, 0, w, h))
	return m.card
}

// Lays the composed face of a card onto the stage at its frame, every cell blended once by its alpha
func (m *Model) stamp(face screen, p placed) {
	if p.alpha >= 1 || m.theme.Plain {
		face.Draw(m.buf, uv.Rect(p.x, p.y, p.w, p.h))
		return
	}
	for y := 0; y < p.h; y++ {
		for x := 0; x < p.w; {
			c := face.CellAt(x, y)
			if c == nil || c.Width == 0 {
				x++
				continue
			}
			m.ink(p.x+x, p.y+y, *c, p.alpha)
			x += c.Width
		}
	}
}

// Draws the tiles, the zoomed and carried tiles over them, then the floats, all shifted by dx
func (m *Model) paintWorkspace(ws *Workspace, dx int, shown bool) {
	if dx <= -m.width || dx >= m.width {
		return
	}
	veil := 0.0
	if m.width > 0 {
		veil = min(math.Abs(ws.shift.Pos)/float64(m.width), 1) * slideFade
	}
	if len(ws.tiles)+len(ws.floats) == 0 {
		m.paintEmpty(dx, veil)
		return
	}
	var held, top *entry
	for _, e := range ws.tiles {
		switch {
		case e.held:
			held = e
		case e == ws.zoom:
			top = e
		default:
			m.paintBody(e, dx, shown, veil)
		}
	}
	if top != nil {
		m.paintBody(top, dx, shown, veil)
	}
	if held != nil {
		m.paintGhost(held, dx)
		m.paintBody(held, dx, shown, veil)
	}
	for _, e := range ws.floats {
		m.paintBody(e, dx, shown, veil)
	}
}

// Dim invitation in the middle of a stage holding no cards, naming the key of every bound hint action
func (m *Model) paintEmpty(dx int, veil float64) {
	t := m.theme
	parts := []string{"nothing open"}
	for _, id := range m.empty {
		a, ok := m.keys.Action(id)
		k := keys.Format(m.keys.First(id))
		if !ok || k == "" {
			continue
		}
		parts = append(parts, k+" "+a.Label)
	}
	text := strings.Join(parts, " "+t.Glyphs.Sep+" ")
	st := m.stage()
	tw := ansi.StringWidth(text)
	if st.W < tw+2 || st.H < 1 {
		return
	}
	x, y := st.X+(st.W-tw)/2+dx, st.Y+st.H/2+barHeight
	label := lipgloss.NewStyle().Background(t.Bg).Foreground(theme.Mix(t.TextDim, t.Bg, veil)).Render(text)
	uv.NewStyledString(label).Draw(m.buf, uv.Rect(x, y, tw, 1))
}

// Faintly lit box with a ghost ring where a carried card will land
func (m *Model) paintGhost(e *entry, dx int) {
	b := e.box
	if b.W < 2 || b.H < 2 {
		return
	}
	t := m.theme
	f := Box{b.X + dx, b.Y + barHeight, b.W, b.H}
	bg := t.Bg
	if !t.Plain {
		bg = theme.Mix(t.Bg, t.Ice, 0.1)
		m.fillBox(f, uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: bg}})
	}
	st := uv.Style{Fg: t.Ghost, Bg: bg}
	m.perimeter(f.X, f.Y, f.W, f.H, func(px, py int, ch string) {
		m.put(m.buf, px, py, ch, st)
	})
}

// Writes one cell over every stage cell of a box
func (m *Model) fillBox(b Box, c uv.Cell) {
	for y := max(b.Y, barHeight); y < min(b.Y+b.H, m.height); y++ {
		for x := max(b.X, 0); x < min(b.X+b.W, m.width); x++ {
			m.buf.SetCell(x, y, &c)
		}
	}
}

// Screen frame of a card this frame with how lit its ring is, its hover and its opacity
type placed struct {
	x, y, w, h int
	lit, hover float64
	alpha      float64
}

// Places a card on screen for this frame, false when nothing of it would show
func (m *Model) place(e *entry, dx int, shown bool, veil float64) (placed, bool) {
	x, y, w, h := e.rect.Cells()
	x += dx + int(math.Round(e.shake.Pos))
	y += barHeight
	if w < 1 || h < 1 || x >= m.width || x+w <= 0 || y >= m.height {
		return placed{}, false
	}
	lit, hover := e.lit.Pos, e.hover.Pos
	if !shown {
		lit = 0
	}
	alpha := 1 - math.Max(math.Max(1-e.open.Pos, e.hide.Pos), veil)
	if alpha <= 0.01 {
		return placed{}, false
	}
	if m.theme.Plain {
		alpha = 1
	}
	return placed{x, y, w, h, lit, hover, alpha}, true
}

// Composes a card as a surface with ring, content and edge text, a glyph chip when too small
func (m *Model) paintBody(e *entry, dx int, shown bool, veil float64) {
	p, ok := m.place(e, dx, shown, veil)
	if !ok {
		return
	}
	t := m.theme
	w, h := p.w, p.h
	face := m.face(w, h)
	if w < 3 || h < 3 {
		m.paintGlyph(face, e, 0, 0, w, h, p.lit)
		m.stamp(face, p)
		return
	}
	m.paintRing(face, e, p)
	f := Box{0, 0, w, h}
	c := m.contentRect(f)
	if c.W < minContentW {
		m.paintGlyph(face, e, 1, 1, w-2, h-2, p.lit)
		m.stamp(face, p)
		return
	}
	m.content(e, c.W, c.H, (1-p.lit)*contentDim).Draw(face, uv.Rect(c.X, c.Y, c.W, c.H))
	right := 0
	if closable(e.card) && w >= minTitleW && e.ws != nil {
		right = 2
		fg := theme.Mix(t.TextDim, t.Text, p.lit)
		if m.hover == e && m.hoverEdge == rClose {
			fg = t.Alarm
		}
		m.put(face, closeX(f), 0, t.Glyphs.Close, uv.Style{Fg: fg, Bg: t.Surface, Attrs: uv.AttrBold})
	}
	sw := m.paintStatus(face, e, w-right, w-6-right-max(ansi.StringWidth(" "+t.Glyphs.Notch+" "+e.card.Title()+" "), minTitleW), p.lit)
	avail := w - 4 - right
	if sw > 0 {
		avail -= sw + 2
	}
	m.paintTitle(face, e, avail, p.lit)
	focused := e.ws == nil || (shown && e.ws.focus == e && m.overlay == nil)
	if focused && !e.closing {
		m.paintHints(face, e, p.x, w, h)
	}
	m.stamp(face, p)
}

// Draws the ring line through the border cells of a face over its surface
func (m *Model) paintRing(face screen, e *entry, p placed) {
	rim := m.rim(e, p.w, p.h, p.lit, p.hover)
	k := 0
	m.perimeter(0, 0, p.w, p.h, func(px, py int, ch string) {
		m.put(face, px, py, ch, uv.Style{Fg: rim(k), Bg: m.theme.Surface})
		k++
	})
}

// Content cells of a card at the given size and dim, rebuilt only when the card, size, or dim changed
func (m *Model) content(e *entry, w, h int, dim float64) *uv.Buffer {
	fresh := e.raw.Buffer == nil
	resized := !fresh && (e.raw.Width() != w || e.raw.Height() != h)
	switch {
	case resized && m.holding(e):
		e.raw.Resize(w, h)
		e.dirty, e.dim, e.view = true, -1, ""
	case fresh || resized || e.dirty:
		if fresh {
			e.raw = screen{uv.NewBuffer(w, h)}
		} else {
			e.raw.Resize(w, h)
		}
		view := e.card.View(w, h)
		if fresh || resized || view != e.view {
			e.raw.Clear()
			uv.NewStyledString(view).Draw(e.raw, e.raw.Bounds())
			e.view, e.dim = view, -1
		}
		e.dirty = false
	}
	if e.dim != dim {
		if e.tinted.Buffer == nil {
			e.tinted = screen{uv.NewBuffer(w, h)}
		} else {
			e.tinted.Resize(w, h)
		}
		for y := range e.raw.Lines {
			copy(e.tinted.Lines[y], e.raw.Lines[y])
		}
		m.tint(e.tinted, dim)
		e.dim = dim
	}
	return e.tinted.Buffer
}

// Reports whether a card keeps its last drawing clipped to its frame while its size moves or the screen just resized
func (m *Model) holding(e *entry) bool {
	return m.settling() || m.motion > 0 && !(e.rect.W.Settled() && e.rect.H.Settled())
}

// Gives content the surface background and fades it toward the surface by dim, keeping links intact
func (m *Model) tint(s screen, dim float64) {
	t := m.theme
	fade := m.memo(func(c color.Color) color.Color { return theme.Mix(c, t.Surface, dim) })
	for _, line := range s.Lines {
		for i := range line {
			c := &line[i]
			c.Style.Fg = or(c.Style.Fg, t.Text)
			c.Style.Bg = or(c.Style.Bg, t.Surface)
			if dim <= 0 || t.Plain {
				continue
			}
			c.Style.Fg = fade(c.Style.Fg)
			if c.Style.Bg != t.Surface {
				c.Style.Bg = fade(c.Style.Bg)
			}
		}
	}
}

// Writes the card's glyph in the middle of a box of the face too small for content
func (m *Model) paintGlyph(face screen, e *entry, x, y, w, h int, lit float64) {
	if w < 1 || h < 1 {
		return
	}
	fg := theme.Mix(m.theme.TextDim, m.theme.Accent, math.Max(lit, 0.35))
	m.put(face, x+w/2, y+h/2, glyphOf(e.card, m.theme), uv.Style{Fg: fg, Bg: m.theme.Surface})
}

// Writes the notch and decoding title into the top edge of the face within avail columns
func (m *Model) paintTitle(face screen, e *entry, avail int, lit float64) {
	if avail < minTitleW {
		return
	}
	t := m.theme
	title := e.card.Title()
	if title == "" {
		return
	}
	shown := []rune(anim.Decode(title, e.decode, m.tick, t.Glyphs.Churn))
	reveal := min(anim.Revealed(title, e.decode), len(shown))
	base := lipgloss.NewStyle().Background(t.Surface)
	notch := t.Glyphs.Notch
	if m.hover == e && m.hoverGrip && (m.hoverEdge == rTitle || m.hoverEdge == rClose) {
		notch = t.Glyphs.Grip
	}
	label := base.Render(" ") +
		base.Foreground(theme.Mix(t.TextDim, t.Accent, lit)).Render(notch+" ") +
		base.Foreground(theme.Mix(t.TextDim, t.Text, lit)).Bold(lit > 0.5).Render(string(shown[:reveal])) +
		base.Foreground(theme.Mix(t.TextDim, t.Ice, lit)).Render(string(shown[reveal:])) +
		base.Render(" ")
	if ansi.StringWidth(label) > avail {
		label = ansi.Truncate(label, avail-1, t.Glyphs.Ellipsis) + base.Render(" ")
	}
	uv.NewStyledString(label).Draw(face, uv.Rect(2, 0, ansi.StringWidth(label), 1))
}

// Writes the status trailer into the top edge when no wider than limit and reports the width used
func (m *Model) paintStatus(face screen, e *entry, w, limit int, lit float64) int {
	s, ok := e.card.(Statuser)
	if !ok {
		return 0
	}
	text := strings.TrimSpace(s.Status())
	if text == "" {
		return 0
	}
	t := m.theme
	if tw := ansi.StringWidth(text); tw+2 > limit {
		if limit < minTitleW {
			return 0
		}
		ew := ansi.StringWidth(t.Glyphs.Ellipsis)
		text = t.Glyphs.Ellipsis + ansi.TruncateLeft(text, tw-(limit-2-ew), "")
	}
	label := lipgloss.NewStyle().Background(t.Surface).Foreground(theme.Mix(t.TextDim, t.Ice, lit)).Render(" " + text + " ")
	lw := ansi.StringWidth(label)
	uv.NewStyledString(label).Draw(face, uv.Rect(w-2-lw, 0, lw, 1))
	return lw
}

// Inverts the cells under the current text sweep
func (m *Model) paintSelection(buf screen) {
	s := m.sel
	if s == nil || !s.moved {
		return
	}
	r := m.contentRect(s.e.frame())
	t := m.theme
	for y := 0; y < r.H; y++ {
		for x := 0; x < r.W; x++ {
			if !s.covers(x, y) {
				continue
			}
			c := buf.CellAt(r.X+x, r.Y+y+barHeight)
			if c == nil {
				continue
			}
			c.Style.Bg = t.Ice
			c.Style.Fg = t.Bg
		}
	}
}

// Darkens every cell but the field a palette types into by the veil share so the overlay stands out
func (m *Model) paintBackdrop(amount float64) {
	if m.theme.Plain {
		return
	}
	dim := backdropDim * amount
	shade := m.memo(func(c color.Color) color.Color { return theme.Shade(c, dim) })
	fx, fw := m.fieldBox()
	open := m.docked() != nil
	for y := 0; y < m.height; y++ {
		for x := 0; x < m.width; x++ {
			field := open && y < barHeight && x >= fx-1 && x <= fx+fw
			if field && x >= fx && x < fx+fw {
				continue
			}
			c := m.buf.CellAt(x, y)
			if c == nil || c.Width == 0 {
				continue
			}
			c.Style.Bg = shade(or(c.Style.Bg, m.theme.Bg))
			if c.Style.Fg != nil && !field {
				c.Style.Fg = shade(c.Style.Fg)
			}
		}
	}
}

// Draws toasts stacked under the bar at the right, sliding and fading in and out at the edge
func (m *Model) paintToasts() {
	if len(m.toasts) == 0 || m.width < 12 {
		return
	}
	t := m.theme
	limit := max(10, m.width/2)
	base := lipgloss.NewStyle().Background(t.Surface)
	for i, tt := range m.toasts {
		y := barHeight + i*toastGap
		if y >= m.height {
			break
		}
		glyph, fg := t.Glyphs.Info, t.Ice
		switch tt.level {
		case Success:
			glyph, fg = t.Glyphs.Success, t.Alive
		case Warn:
			glyph, fg = t.Glyphs.Warn, t.Accent
		case Error:
			glyph, fg = t.Glyphs.Fail, t.Alarm
		}
		text := ansi.Truncate(tt.text, limit-4, t.Glyphs.Ellipsis)
		label := base.Render(" ") + base.Foreground(fg).Bold(true).Render(glyph) + base.Render(" ") + base.Foreground(t.Text).Render(text) + base.Render(" ")
		lw := ansi.StringWidth(label)
		slide := min(max(tt.x.Pos, 0), 1)
		x := m.width - m.gapOutX - lw + int(math.Round(tt.x.Pos*float64(lw+m.gapOutX)))
		alpha := 1 - slide
		m.pad(x-1, y, lw+2, alpha)
		face := m.face(lw, 1)
		uv.NewStyledString(label).Draw(face, uv.Rect(0, 0, lw, 1))
		m.stamp(face, placed{x: x, y: y, w: lw, h: 1, alpha: alpha})
	}
}

// Pads a one row surface with a blank surface cell at each end
func (m *Model) pad(x, y, w int, alpha float64) {
	for _, cx := range []int{x, x + w - 1} {
		if cx < 0 || cx >= m.width || y < barHeight || y >= m.height {
			continue
		}
		m.ink(cx, y, uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: m.theme.Surface}}, alpha)
	}
}

// Visits every border cell clockwise from the top left corner with its box drawing character
func (m *Model) perimeter(x, y, w, h int, fn func(px, py int, ch string)) {
	b := m.theme.Border
	for i := range w {
		ch := b.Top
		switch i {
		case 0:
			ch = b.TopLeft
		case w - 1:
			ch = b.TopRight
		}
		fn(x+i, y, ch)
	}
	for j := 1; j < h-1; j++ {
		fn(x+w-1, y+j, b.Right)
	}
	for i := w - 1; i >= 0; i-- {
		ch := b.Bottom
		switch i {
		case w - 1:
			ch = b.BottomRight
		case 0:
			ch = b.BottomLeft
		}
		fn(x+i, y+h-1, ch)
	}
	for j := h - 2; j >= 1; j-- {
		fn(x, y+j, b.Left)
	}
}

// Border color at perimeter index k, lit by focus with corner brackets, an urgency pulse and a hovered grip
func (m *Model) rim(e *entry, w, h int, lit, hover float64) func(k int) color.Color {
	t := m.theme
	edge := theme.Mix(t.Steel, t.SteelBright, hover)
	if e.urgency != Calm && e.pulse.Pos > 0 {
		edge = theme.Mix(edge, m.urgencyColor(e.urgency), e.pulse.Pos*m.beat())
	}
	grip := rNone
	if m.hover == e && m.hoverGrip {
		grip = m.hoverEdge
		if grip == rClose {
			grip = rTitle
		}
	}
	kp := -1
	if grip == rLeft || grip == rRight || grip == rBottom {
		f := e.frame()
		kp = perimIndex(w, h, m.hoverX-f.X, m.hoverY-f.Y)
	}
	hot := func(k int, c color.Color) color.Color {
		if grip == rNone || sideOf(k, w, h) != grip {
			return c
		}
		amount := 0.5 + 0.5*hover
		if kp >= 0 {
			amount *= 0.25 + 0.75*math.Exp(-math.Abs(float64(k-kp))/gripReach)
		}
		return theme.Mix(c, t.Ice, amount)
	}
	// The cascade only lights the focused card, and only where the theme can show it
	cascade := m.cascade && e == m.focused() && !t.Plain && m.motion > 0
	cascadeHead := int(m.phase * cascadeSpeed)
	blend := func(k int, c color.Color) color.Color {
		c = hot(k, c)
		if !cascade {
			return c
		}
		return theme.Mix(c, t.Ice, cascadeIntensity(2*(w+h)-4, k, cascadeHead)*cascadeBreath)
	}
	if lit <= 0 {
		return func(k int) color.Color { return blend(k, edge) }
	}
	ring := m.ring(2*(w+h) - 4)
	offset := int(m.phase)
	return func(k int) color.Color {
		c := ring[(k+offset)%len(ring)]
		if lit < 1 {
			c = theme.Mix(edge, c, lit)
		}
		if bracket(k, w, h) {
			c = theme.Mix(c, t.Accent, lit)
		}
		return blend(k, c)
	}
}

// Reports whether perimeter index k lies on the top left or bottom right corner bracket of the frame
func bracket(k, w, h int) bool {
	perim := 2*(w+h) - 4
	arm := min(3, w-1)
	corner := w + h - 2
	switch {
	case k < arm, k >= corner && k < corner+arm:
		return true
	case h > 2 && (k == perim-1 || k == corner-1):
		return true
	}
	return false
}

// Share of an urgency pulse lit right now, full when motion is off
func (m *Model) beat() float64 {
	if m.motion == 0 {
		return 1
	}
	return 0.55 + 0.45*math.Sin(m.phase*pulseSpeed)
}

// Cascade intensity at perimeter index k, brightest at the comet head and fading along its tail
func cascadeIntensity(perim, k, head int) float64 {
	if perim <= 0 {
		return 0
	}
	d := k - head
	d = ((d % perim) + perim) % perim
	if d > perim/2 {
		d = perim - d
	}
	return math.Exp(-float64(d) / cascadeTail)
}

// Clockwise perimeter index of the border cell nearest a point in frame coordinates
func perimIndex(w, h, px, py int) int {
	perim := max(2*(w+h)-4, 1)
	px, py = min(max(px, 0), w-1), min(max(py, 0), h-1)
	top, bottom, left, right := py, h-1-py, px, w-1-px
	switch nearest := min(top, bottom, left, right); {
	case nearest == top:
		return px % perim
	case nearest == right:
		return (w - 1 + py) % perim
	case nearest == bottom:
		return (2*w + h - 3 - px) % perim
	}
	return (2*w + 2*h - 4 - py) % perim
}

func (m *Model) urgencyColor(u Urgency) color.Color {
	if u == Alarm {
		return m.theme.Alarm
	}
	return m.theme.Pulse
}

// Gradient ring of n colors for a border of that perimeter, cached per size
func (m *Model) ring(n int) []color.Color {
	n = max(n, 1)
	if r, ok := m.rings[n]; ok {
		return r
	}
	var r []color.Color
	if m.theme.Plain {
		r = []color.Color{m.theme.Ice}
	} else {
		r = theme.Ring(n, m.theme.Ring...)
	}
	if len(r) == 0 {
		r = []color.Color{m.theme.Ice}
	}
	m.rings[n] = r
	return r
}

// Terminal cursor of the palette input, else of the overlay or focused card in screen space, nil when hidden
func (m *Model) cursor() *tea.Cursor {
	if m.docked() != nil {
		if m.caret < 0 {
			return nil
		}
		return tea.NewCursor(m.caret, 0)
	}
	e := m.overlay
	if e == nil {
		e = m.focused()
	}
	if e == nil {
		return nil
	}
	cz, ok := e.card.(Cursorer)
	if !ok {
		return nil
	}
	c := cz.Cursor()
	if c == nil {
		return nil
	}
	r := m.contentRect(e.frame())
	if r.W < minContentW || r.H < 1 {
		return nil
	}
	out := *c
	out.X, out.Y = r.X+c.X, r.Y+barHeight+c.Y
	if !r.contains(out.X, out.Y-barHeight) {
		return nil
	}
	return &out
}

// Writes one styled grapheme, reusing the measured cell for that grapheme
func (m *Model) put(buf screen, x, y int, s string, st uv.Style) {
	if x < 0 || y < 0 {
		return
	}
	c, ok := m.glyphs[s]
	if !ok {
		nc := uv.NewCell(buf.WidthMethod(), s)
		if nc == nil {
			return
		}
		c = *nc
		m.glyphs[s] = c
	}
	c.Style = st
	buf.SetCell(x, y, &c)
}

// Wraps a color transform so each distinct input is computed once per paint pass
func (m *Model) memo(fn func(color.Color) color.Color) func(color.Color) color.Color {
	clear(m.memoed)
	return func(c color.Color) color.Color {
		if out, ok := m.memoed[c]; ok {
			return out
		}
		out := fn(c)
		m.memoed[c] = out
		return out
	}
}

func or(c, fallback color.Color) color.Color {
	if c == nil {
		return fallback
	}
	return c
}

// Cell buffer that measures text by grapheme width so it matches lipgloss
type screen struct {
	*uv.Buffer
}

// Reuses the cell buffer between frames, reallocating only when the screen changes size
func (m *Model) frame() screen {
	if m.buf.Buffer == nil || m.buf.Width() != m.width || m.buf.Height() != m.height {
		m.buf = screen{uv.NewBuffer(m.width, m.height)}
	}
	return m.buf
}

func (screen) WidthMethod() uv.WidthMethod { return uv.WidthMethod(ansi.GraphemeWidth) }

// Writes the card's bound keys into the bottom edge, skipping those that do not fit, lighting the hovered one
func (m *Model) paintHints(face screen, e *entry, sx, w, h int) {
	e.hints = e.hints[:0]
	hints := append([]Hint(nil), hintsOf(e.card)...)
	if ctxs := contextsOf(e.card); e.ws != nil && closable(e.card) {
		switch {
		case slices.Contains(ctxs, keys.Panel):
			hints = append(hints, Hint{Action: "panel.close"})
		case ctxs == nil:
			hints = append(hints, Hint{Action: "window.close"})
		}
	}
	avail := w - 4
	if len(hints) == 0 || avail < minTitleW {
		return
	}
	t := m.theme
	base := lipgloss.NewStyle().Background(t.Surface)
	var sb strings.Builder
	sb.WriteString(base.Render(" "))
	col := sx + 2 + 1
	used := 1
	for _, h := range hints {
		key, label := m.hintText(h)
		if key == "" {
			continue
		}
		sep := ""
		if len(e.hints) > 0 {
			sep = base.Foreground(t.Steel).Render(" " + t.Glyphs.Sep + " ")
		}
		hot := m.hover == e && m.hintHot == h.Action && h.Action != ""
		text := base.Foreground(t.Ice).Bold(hot).Render(key)
		if label != "" {
			labelFg := t.TextDim
			if hot {
				labelFg = t.Text
			}
			text += base.Foreground(labelFg).Render(" " + label)
		}
		width := ansi.StringWidth(text)
		if used+ansi.StringWidth(sep)+width+1 > avail {
			continue
		}
		sb.WriteString(sep)
		col += ansi.StringWidth(sep)
		e.hints = append(e.hints, hintSpan{x0: col, x1: col + width, id: h.Action})
		sb.WriteString(text)
		col += width
		used += ansi.StringWidth(sep) + width
	}
	if len(e.hints) == 0 {
		return
	}
	sb.WriteString(base.Render(" "))
	uv.NewStyledString(sb.String()).Draw(face, uv.Rect(2, h-1, used+1, 1))
}

// Key and label a hint paints, the key empty when its action is unbound
func (m *Model) hintText(h Hint) (string, string) {
	key := m.keys.First(h.Action)
	if h.Pair != "" {
		key = keys.Pair(m.keys.First(h.Action), m.keys.First(h.Pair))
	} else {
		key = keys.Format(key)
	}
	if key == "" {
		return "", ""
	}
	label := h.Label
	if label == "" {
		if a, ok := m.keys.Action(h.Action); ok {
			label = a.Label
		}
	}
	return key, label
}

// Frame edge a clockwise perimeter index falls on
func sideOf(k, w, h int) region {
	switch {
	case k < w:
		return rTitle
	case k < w+h-2:
		return rRight
	case k < 2*w+h-2:
		return rBottom
	}
	return rLeft
}
