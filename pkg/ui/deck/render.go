package deck

import (
	"image/color"
	"math"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/fx"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

const (
	minContentW = 4
	minTitleW   = 8
	contentDim  = 0.3
	emptyLabel  = "no cards"
)

type layer struct {
	e       *entry
	z       int
	ord     int
	leaving bool
}

// Composites the deck into a full screen view with mouse tracking and the terminal background set
func (m *Model) View() tea.View {
	buf := m.frame()
	buf.Fill(&uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: m.theme.Bg}})
	m.paintGutter(buf)
	m.paintEmpty(buf)
	layers := m.layers()
	for _, l := range layers {
		if !l.leaving && l.e.rect.Distance() > liftDistance {
			m.paintGhost(buf, l.e)
		}
	}
	for _, l := range layers {
		m.paintCard(buf, l.e, l.ord)
	}
	m.paintOverflow(buf)
	m.paintSelection(buf)
	if m.overlay != nil {
		m.paintBackdrop(buf)
		m.paintCard(buf, m.overlay, -1)
	}
	m.paintToasts(buf)
	m.paintHints(buf)
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

// Live and departing cards sorted so higher stacking ranks paint last
func (m *Model) layers() []layer {
	n := len(m.cards)
	out := make([]layer, 0, n+len(m.leaving))
	for i, e := range m.cards {
		out = append(out, layer{e: e, z: m.z(e, i, n), ord: i})
	}
	for i, e := range m.leaving {
		out = append(out, layer{e: e, z: 2*n + 10 + i, ord: -1, leaving: true})
	}
	slices.SortStableFunc(out, func(a, b layer) int { return a.z - b.z })
	return out
}

func (m *Model) paintGutter(buf screen) {
	if m.mode != Split || len(m.cards) < 2 || len(m.boxes) == 0 || m.width < narrowWidth {
		return
	}
	t := m.theme
	x := m.boxes[0].x + m.boxes[0].w
	ch, fg := "┆", t.Steel
	if t.Plain {
		ch = ":"
	}
	switch {
	case m.dragging:
		ch, fg = "┃", t.Ice
		if t.Plain {
			ch = "|"
		}
	case m.gutterHot:
		ch, fg = "┃", t.SteelBright
		if t.Plain {
			ch = "|"
		}
	}
	for y := 0; y < m.stageHeight(); y++ {
		m.put(buf, x, y, ch, uv.Style{Fg: fg, Bg: t.Bg})
	}
}

// Centers a quiet label on the stage when the deck holds no cards
func (m *Model) paintEmpty(buf screen) {
	w, h := m.width, m.stageHeight()
	if len(m.cards) > 0 || w < len(emptyLabel) || h < 1 {
		return
	}
	label := lipgloss.NewStyle().Foreground(m.theme.TextDim).Background(m.theme.Bg).Render(emptyLabel)
	uv.NewStyledString(label).Draw(buf, uv.Rect((w-len(emptyLabel))/2, h/2, len(emptyLabel), 1))
}

// Faint outline where a card in flight will land
func (m *Model) paintGhost(buf screen, e *entry) {
	x, y, w, h := e.rect.Target()
	if w < 2 || h < 2 {
		return
	}
	st := uv.Style{Fg: m.theme.Ghost, Bg: m.theme.Bg}
	m.perimeter(x, y, w, h, func(px, py int, ch string) { m.put(buf, px, py, ch, st) })
}

// Draws one card as a framed box, a folded title strip, or a glyph chip depending on its size
func (m *Model) paintCard(buf screen, e *entry, ord int) {
	x, y, w, h := e.rect.Cells()
	if w < 1 || h < 1 {
		return
	}
	t := m.theme
	glow, hover, lift := e.glow.Pos, e.hover.Pos, e.lift.Pos
	m.paintShadow(buf, x, y, w, h, lift)
	buf.FillArea(&uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: t.Surface}}, uv.Rect(x, y, w, h))
	rim := m.rim(e, w, h, glow, hover)
	switch {
	case h < 3 && w >= minTitleW:
		m.paintStrip(buf, e, ord, x, y, w, h, glow, rim)
		return
	case w < 3 || h < 3:
		m.paintGlyph(buf, e, x, y, w, h, glow)
		return
	}
	k := 0
	m.perimeter(x, y, w, h, func(px, py int, ch string) {
		m.put(buf, px, py, ch, uv.Style{Fg: rim(k), Bg: t.Surface})
		k++
	})
	iw, ih := w-2, h-2
	if iw < minContentW {
		m.paintGlyph(buf, e, x+1, y+1, iw, ih, glow)
		return
	}
	dim := math.Max((1-glow)*contentDim, e.fade)
	m.content(e, iw, ih, dim).Draw(buf, uv.Rect(x+1, y+1, iw, ih))
	m.paintTitle(buf, e, ord, x, y, w, glow)
	m.paintStatus(buf, e, x, y+h-1, w, w-4, glow)
}

// Border color at clockwise perimeter index k, flowing the focus ring in over the steel edge and pulsing with any urgency
func (m *Model) rim(e *entry, w, h int, glow, hover float64) func(k int) color.Color {
	t := m.theme
	edge := theme.Mix(t.Steel, t.SteelBright, hover)
	if e.urgency != Calm && e.pulse.Pos > 0 {
		beat := 0.55 + 0.45*math.Sin(m.phase*pulseSpeed)
		if m.motion == 0 {
			beat = 1
		}
		edge = theme.Mix(edge, m.urgencyColor(e.urgency), e.pulse.Pos*beat)
	}
	if glow <= 0 {
		return func(int) color.Color { return edge }
	}
	ring := m.ring(2*(w+h) - 4)
	offset := int(m.phase)
	return func(k int) color.Color {
		c := ring[(k+offset)%len(ring)]
		if glow >= 1 {
			return c
		}
		return theme.Mix(edge, c, glow)
	}
}

func (m *Model) urgencyColor(u Urgency) color.Color {
	if u == Alarm {
		return m.theme.Alarm
	}
	return m.theme.Pulse
}

// Draws a folded card as a title and status edge, adding the bottom edge when two rows tall
func (m *Model) paintStrip(buf screen, e *entry, ord, x, y, w, h int, glow float64, rim func(int) color.Color) {
	b, t := m.theme.Border, m.theme
	for i := 0; i < w; i++ {
		top, bottom := b.Top, b.Bottom
		switch i {
		case 0:
			top, bottom = b.TopLeft, b.BottomLeft
		case w - 1:
			top, bottom = b.TopRight, b.BottomRight
		}
		m.put(buf, x+i, y, top, uv.Style{Fg: rim(i), Bg: t.Surface})
		if h > 1 {
			m.put(buf, x+i, y+h-1, bottom, uv.Style{Fg: rim(2*w - 1 - i), Bg: t.Surface})
		}
	}
	avail := w
	if sw := m.paintStatus(buf, e, x, y, w, min(w-4, w-1-minTitleW), glow); sw > 0 {
		avail = w - sw - 1
	}
	m.paintTitle(buf, e, ord, x, y, avail, glow)
}

// Darkens whatever lies below and to the right of a card, reaching further while it is lifted
func (m *Model) paintShadow(buf screen, x, y, w, h int, lift float64) {
	if m.theme.Shadow <= 0 {
		return
	}
	dx := 1 + int(math.Round(lift))
	amount := m.theme.Shadow * (0.6 + 0.4*lift)
	shade := m.memo(func(c color.Color) color.Color { return theme.Shade(c, amount) })
	for py := y + 1; py < y+h+1; py++ {
		for px := x + dx; px < x+w+dx; px++ {
			if px >= x && px < x+w && py < y+h {
				continue
			}
			c := buf.CellAt(px, py)
			if c == nil {
				continue
			}
			c.Style.Bg = shade(or(c.Style.Bg, m.theme.Bg))
			if c.Style.Fg != nil {
				c.Style.Fg = shade(c.Style.Fg)
			}
		}
	}
}

// Content cells of a card at the given size and dim, rebuilt only when the card, size, or dim changed
func (m *Model) content(e *entry, w, h int, dim float64) *uv.Buffer {
	if e.dirty || e.raw.Buffer == nil || e.raw.Width() != w || e.raw.Height() != h {
		if e.raw.Buffer == nil {
			e.raw = screen{uv.NewBuffer(w, h)}
		} else {
			e.raw.Resize(w, h)
		}
		e.raw.Clear()
		uv.NewStyledString(e.card.View(w, h)).Draw(e.raw, e.raw.Bounds())
		e.cw, e.ch = w, h
		e.dirty, e.dim = false, -1
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

func (m *Model) paintGlyph(buf screen, e *entry, x, y, w, h int, glow float64) {
	if w < 1 || h < 1 {
		return
	}
	fg := theme.Mix(m.theme.TextDim, m.theme.Accent, math.Max(glow, 0.35))
	m.put(buf, x+w/2, y+h/2, e.card.Glyph(), uv.Style{Fg: fg, Bg: m.theme.Surface})
}

// Writes the ordinal, glyph, and decoding title into the top edge within avail columns
func (m *Model) paintTitle(buf screen, e *entry, ord, x, y, avail int, glow float64) {
	if avail < minTitleW {
		return
	}
	t := m.theme
	title := e.card.Title()
	shown := []rune(fx.Decode(title, e.decode, m.tick))
	reveal := min(fx.Revealed(title, e.decode), len(shown))
	base := lipgloss.NewStyle().Background(t.Surface)
	label := base.Render(" ")
	if m.hints && ord >= 0 && ord < 9 && len(m.cards) > 1 {
		label += base.Foreground(theme.Mix(t.TextDim, t.Ice, glow)).Render(strconv.Itoa(ord+1) + " ")
	}
	label += base.Foreground(theme.Mix(t.TextDim, t.Accent, glow)).Render(e.card.Glyph()+" ") +
		base.Foreground(theme.Mix(t.TextDim, t.Text, glow)).Bold(glow > 0.5).Render(string(shown[:reveal])) +
		base.Foreground(theme.Mix(t.TextDim, t.Ice, glow)).Render(string(shown[reveal:])) +
		base.Render(" ")
	label = ansi.Truncate(label, avail-4, "")
	uv.NewStyledString(label).Draw(buf, uv.Rect(x+2, y, ansi.StringWidth(label), 1))
}

// Writes the status trailer into edge row y when no wider than limit and reports the width used
func (m *Model) paintStatus(buf screen, e *entry, x, y, w, limit int, glow float64) int {
	s, ok := e.card.(Statuser)
	if !ok {
		return 0
	}
	text := strings.TrimSpace(s.Status())
	if text == "" {
		return 0
	}
	t := m.theme
	label := lipgloss.NewStyle().Background(t.Surface).Foreground(theme.Mix(t.TextDim, t.Ice, glow)).Render(" " + text + " ")
	lw := ansi.StringWidth(label)
	if lw > limit {
		return 0
	}
	uv.NewStyledString(label).Draw(buf, uv.Rect(x+w-2-lw, y, lw, 1))
	return lw
}

// Marks how many stack strips scrolled off the top or bottom of the stage
func (m *Model) paintOverflow(buf screen) {
	if m.mode != Split || len(m.cards) < 2 || len(m.boxes) < 2 {
		return
	}
	above, below := 0, 0
	stage := m.stageHeight()
	start := 1
	if m.width < narrowWidth {
		start = 0
	}
	for i := start; i < len(m.cards); i++ {
		_, y, _, h := m.cards[i].rect.Target()
		switch {
		case y+h <= 0:
			above++
		case y >= stage:
			below++
		}
	}
	if above == 0 && below == 0 {
		return
	}
	t := m.theme
	st := lipgloss.NewStyle().Background(t.Bg).Foreground(t.Accent)
	bx, bw := m.boxes[start].x, m.boxes[start].w
	draw := func(text string, y int) {
		label := st.Render(" " + text + " ")
		lw := ansi.StringWidth(label)
		if lw > bw {
			return
		}
		uv.NewStyledString(label).Draw(buf, uv.Rect(bx+bw-lw, y, lw, 1))
	}
	up, down := "▲", "▼"
	if t.Plain {
		up, down = "^", "v"
	}
	if above > 0 {
		draw(up+" "+strconv.Itoa(above)+" above", 0)
	}
	if below > 0 && stage > 1 {
		draw(down+" "+strconv.Itoa(below)+" below", stage-1)
	}
}

// Inverts the cells under the current text sweep
func (m *Model) paintSelection(buf screen) {
	s := m.sel
	if s == nil || !s.moved {
		return
	}
	cx, cy, cw, ch := s.e.rect.Cells()
	t := m.theme
	for y := 0; y < ch-2; y++ {
		for x := 0; x < cw-2; x++ {
			if !s.covers(x, y) {
				continue
			}
			c := buf.CellAt(cx+1+x, cy+1+y)
			if c == nil {
				continue
			}
			c.Style.Bg = t.Ice
			c.Style.Fg = t.Bg
		}
	}
}

// Darkens the whole stage so the overlay stands out
func (m *Model) paintBackdrop(buf screen) {
	if m.theme.Plain {
		return
	}
	shade := m.memo(func(c color.Color) color.Color { return theme.Shade(c, backdropDim) })
	for y := 0; y < m.stageHeight(); y++ {
		for x := 0; x < m.width; x++ {
			c := buf.CellAt(x, y)
			if c == nil {
				continue
			}
			c.Style.Bg = shade(or(c.Style.Bg, m.theme.Bg))
			if c.Style.Fg != nil {
				c.Style.Fg = shade(c.Style.Fg)
			}
		}
	}
}

// Draws toasts stacked at the top right, sliding in from the edge
func (m *Model) paintToasts(buf screen) {
	if len(m.toasts) == 0 || m.width < 12 {
		return
	}
	t := m.theme
	limit := max(10, m.width/2)
	for i, tt := range m.toasts {
		if i >= m.stageHeight() {
			break
		}
		glyph, fg := "◆", t.Ice
		switch tt.level {
		case Success:
			glyph, fg = "✓", t.Alive
		case Warn:
			glyph, fg = "▲", t.Accent
		case Error:
			glyph, fg = "✗", t.Alarm
		}
		if t.Plain {
			glyph = "*"
		}
		text := ansi.Truncate(tt.text, limit-4, "…")
		label := lipgloss.NewStyle().Background(t.Surface).Foreground(fg).Bold(true).Render(" "+glyph+" ") + lipgloss.NewStyle().Background(t.Surface).Foreground(t.Text).Render(text+" ")
		lw := ansi.StringWidth(label)
		x := m.width - 1 - lw + int(math.Round(tt.x.Pos*float64(lw+1)))
		uv.NewStyledString(label).Draw(buf, uv.Rect(x, i, lw, 1))
	}
}

// Visits every border cell clockwise from the top left corner with its box drawing character
func (m *Model) perimeter(x, y, w, h int, fn func(px, py int, ch string)) {
	b := m.theme.Border
	for i := 0; i < w; i++ {
		ch := b.Top
		if i == 0 {
			ch = b.TopLeft
		} else if i == w-1 {
			ch = b.TopRight
		}
		fn(x+i, y, ch)
	}
	for j := 1; j < h-1; j++ {
		fn(x+w-1, y+j, b.Right)
	}
	for i := w - 1; i >= 0; i-- {
		ch := b.Bottom
		if i == w-1 {
			ch = b.BottomRight
		} else if i == 0 {
			ch = b.BottomLeft
		}
		fn(x+i, y+h-1, ch)
	}
	for j := h - 2; j >= 1; j-- {
		fn(x, y+j, b.Left)
	}
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
		r = theme.Ring(n, m.theme.Glow...)
	}
	if len(r) == 0 {
		r = []color.Color{m.theme.Ice}
	}
	m.rings[n] = r
	return r
}

// Terminal cursor of the overlay or the focused card translated into screen space, or nil when hidden
func (m *Model) cursor() *tea.Cursor {
	e := m.overlay
	if e == nil {
		if len(m.cards) == 0 {
			return nil
		}
		e = m.cards[m.focus]
	}
	cz, ok := e.card.(Cursorer)
	if !ok {
		return nil
	}
	c := cz.Cursor()
	if c == nil {
		return nil
	}
	x, y, w, h := e.rect.Cells()
	if w-2 < minContentW || h < 3 {
		return nil
	}
	out := *c
	out.X, out.Y = x+1+c.X, y+1+c.Y
	if out.X < x+1 || out.X >= x+w-1 || out.Y < y+1 || out.Y >= y+h-1 {
		return nil
	}
	return &out
}

// Writes one styled grapheme, reusing the measured cell for that grapheme
func (m *Model) put(buf screen, x, y int, s string, st uv.Style) {
	c, ok := m.cells[s]
	if !ok {
		nc := uv.NewCell(buf.WidthMethod(), s)
		if nc == nil {
			return
		}
		c = *nc
		m.cells[s] = c
	}
	c.Style = st
	buf.SetCell(x, y, &c)
}

// Wraps a color transform so each distinct input is computed once per paint pass
func (m *Model) memo(fn func(color.Color) color.Color) func(color.Color) color.Color {
	clear(m.fade)
	return func(c color.Color) color.Color {
		if out, ok := m.fade[c]; ok {
			return out
		}
		out := fn(c)
		m.fade[c] = out
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

// Reuses the cell buffer between frames, reallocating only when the deck changes size
func (m *Model) frame() screen {
	if m.buf.Buffer == nil || m.buf.Width() != m.width || m.buf.Height() != m.height {
		m.buf = screen{uv.NewBuffer(m.width, m.height)}
	}
	return m.buf
}

func (screen) WidthMethod() uv.WidthMethod { return uv.WidthMethod(ansi.GraphemeWidth) }
