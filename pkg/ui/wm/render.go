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

	"github.com/nickheyer/icebath/pkg/ui/fx"
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

// Composites the bar, the stage and the overlay into a full screen view with mouse tracking
func (m *Model) View() tea.View {
	buf := m.frame()
	buf.Fill(&uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: m.theme.Bg}})
	m.paintBar(buf)
	if m.leaving != nil {
		m.paintWorkspace(buf, m.leaving, int(math.Round(m.leaving.shift.Pos)), false)
	}
	if ws := m.Active(); ws != nil {
		m.paintWorkspace(buf, ws, int(math.Round(ws.shift.Pos)), true)
	}
	m.paintSelection(buf)
	if m.overlay != nil {
		m.placeOverlay()
		m.paintBackdrop(buf)
		m.paintEntry(buf, m.overlay, 0, true)
	}
	m.paintToasts(buf)
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

// Draws the tiles then the floats of a workspace shifted by dx, the held card last
func (m *Model) paintWorkspace(buf screen, ws *Workspace, dx int, shown bool) {
	if dx <= -m.width || dx >= m.width {
		return
	}
	var held *entry
	for _, e := range ws.tiles {
		if ws.zoom != nil && e != ws.zoom && !e.closing {
			continue
		}
		if e.held {
			held = e
			continue
		}
		m.paintEntry(buf, e, dx, shown)
	}
	for _, e := range ws.floats {
		if e.held {
			held = e
			continue
		}
		m.paintEntry(buf, e, dx, shown)
	}
	if held != nil {
		m.paintGhost(buf, held, dx)
		m.paintEntry(buf, held, dx, shown)
	}
}

// Faint outline where a carried card will land
func (m *Model) paintGhost(buf screen, e *entry, dx int) {
	b := e.box
	if b.W < 2 || b.H < 2 {
		return
	}
	st := uv.Style{Fg: m.theme.Ghost, Bg: m.theme.Bg}
	m.perimeter(b.X+dx, b.Y+barHeight, b.W, b.H, func(px, py int, ch string) { m.put(buf, px, py, ch, st) })
}

// Draws one card as a framed box, or as a glyph chip when too small for a frame
func (m *Model) paintEntry(buf screen, e *entry, dx int, shown bool) {
	x, y, w, h := e.rect.Cells()
	x += dx
	y += barHeight
	if w < 1 || h < 1 || x >= m.width || x+w <= 0 {
		return
	}
	t := m.theme
	glow, hover := e.glow.Pos, e.hover.Pos
	if !shown {
		glow = 0
	}
	fade := 1 - e.open.Pos
	m.paintShadow(buf, x, y, w, h, e.lift.Pos)
	buf.FillArea(&uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: t.Surface}}, uv.Rect(x, y, w, h))
	rim := m.rim(e, w, h, glow, hover, fade)
	if w < 3 || h < 3 {
		m.paintGlyph(buf, e, x, y, w, h, glow)
		return
	}
	k := 0
	m.perimeter(x, y, w, h, func(px, py int, ch string) {
		m.put(buf, px, py, ch, uv.Style{Fg: rim(k), Bg: t.Surface})
		k++
	})
	c := m.contentRect(Box{x, y, w, h})
	if c.W < minContentW {
		m.paintGlyph(buf, e, x+1, y+1, w-2, h-2, glow)
		return
	}
	dim := math.Max((1-glow)*contentDim, fade)
	m.content(e, c.W, c.H, dim).Draw(buf, uv.Rect(c.X, c.Y, c.W, c.H))
	right := 0
	if closable(e.card) && w >= minTitleW && e.ws != nil {
		right = 2
		fg := theme.Mix(t.TextDim, t.Text, glow)
		if m.hover == e && m.hoverEdge == rClose {
			fg = t.Alarm
		}
		m.put(buf, closeX(Box{x, y, w, h}), y, "×", uv.Style{Fg: fg, Bg: t.Surface, Attrs: uv.AttrBold})
	}
	sw := m.paintStatus(buf, e, x, y, w-right, w-6-right-max(ansi.StringWidth(" "+glyphOf(e.card)+" "+e.card.Title()+" "), minTitleW), glow)
	avail := w - 4 - right
	if sw > 0 {
		avail -= sw + 2
	}
	m.paintTitle(buf, e, x, y, avail, glow)
	focused := e.ws == nil || (shown && e.ws.focus == e && m.overlay == nil)
	if focused && !e.closing {
		m.paintHints(buf, e, x, y+h-1, w)
	}
}

// Border color at clockwise perimeter index k, flowing the focus ring in over the steel edge and pulsing with any urgency
func (m *Model) rim(e *entry, w, h int, glow, hover, fade float64) func(k int) color.Color {
	t := m.theme
	edge := theme.Mix(t.Steel, t.SteelBright, hover)
	if e.urgency != Calm && e.pulse.Pos > 0 {
		beat := 0.55 + 0.45*math.Sin(m.phase*pulseSpeed)
		if m.motion == 0 {
			beat = 1
		}
		edge = theme.Mix(edge, m.urgencyColor(e.urgency), e.pulse.Pos*beat)
	}
	if fade > 0 && !t.Plain {
		edge = theme.Mix(edge, t.Surface, fade)
	}
	grip := rNone
	if m.hover == e && m.hoverGrip {
		grip = m.hoverEdge
		if grip == rClose {
			grip = rTitle
		}
	}
	lit := func(k int, c color.Color) color.Color {
		if grip != rNone && sideOf(k, w, h) == grip {
			return theme.Mix(c, t.Ice, 0.5+0.5*hover)
		}
		return c
	}
	// The cascade only lights the focused card, and only where the theme can show it
	cascade := m.cascade && e == m.focused() && !t.Plain && m.motion > 0
	cascadeHead := int(m.phase * cascadeSpeed)
	blend := func(k int, c color.Color) color.Color {
		c = lit(k, c)
		if !cascade {
			return c
		}
		return theme.Mix(c, t.Ice, cascadeIntensity(2*(w+h)-4, k, cascadeHead)*cascadeBreath)
	}
	if glow <= 0 {
		return func(k int) color.Color { return blend(k, edge) }
	}
	ring := m.ring(2*(w+h) - 4)
	offset := int(m.phase)
	return func(k int) color.Color {
		c := ring[(k+offset)%len(ring)]
		if fade > 0 && !t.Plain {
			c = theme.Mix(c, t.Surface, fade)
		}
		if glow < 1 {
			c = theme.Mix(edge, c, glow)
		}
		return blend(k, c)
	}
}

// Cascade intensity at perimeter index k as a comet sweeps the ring, brightest at the head and fading along its tail
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

func (m *Model) urgencyColor(u Urgency) color.Color {
	if u == Alarm {
		return m.theme.Alarm
	}
	return m.theme.Pulse
}

// Darkens what lies below and right of a lifted card, reaching further as it rises
func (m *Model) paintShadow(buf screen, x, y, w, h int, lift float64) {
	if m.theme.Shadow <= 0 || lift <= 0 {
		return
	}
	dx := 1 + int(math.Round(lift))
	amount := m.theme.Shadow * (0.6 + 0.4*lift)
	shade := m.memo(func(c color.Color) color.Color { return theme.Shade(c, amount) })
	for py := y + 1; py < y+h+1 && py < m.height; py++ {
		for px := x + dx; px < x+w+dx && px < m.width; px++ {
			if px < 0 || py < barHeight || (px >= x && px < x+w && py < y+h) {
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
	m.put(buf, x+w/2, y+h/2, glyphOf(e.card), uv.Style{Fg: fg, Bg: m.theme.Surface})
}

// Writes the glyph and decoding title into the top edge within avail columns
func (m *Model) paintTitle(buf screen, e *entry, x, y, avail int, glow float64) {
	if avail < minTitleW {
		return
	}
	t := m.theme
	title := e.card.Title()
	if title == "" {
		return
	}
	shown := []rune(fx.Decode(title, e.decode, m.tick))
	reveal := min(fx.Revealed(title, e.decode), len(shown))
	base := lipgloss.NewStyle().Background(t.Surface)
	glyph := glyphOf(e.card)
	if m.hover == e && m.hoverGrip && (m.hoverEdge == rTitle || m.hoverEdge == rClose) && !t.Plain {
		glyph = "✥"
	}
	label := base.Render(" ") +
		base.Foreground(theme.Mix(t.TextDim, t.Accent, glow)).Render(glyph+" ") +
		base.Foreground(theme.Mix(t.TextDim, t.Text, glow)).Bold(glow > 0.5).Render(string(shown[:reveal])) +
		base.Foreground(theme.Mix(t.TextDim, t.Ice, glow)).Render(string(shown[reveal:])) +
		base.Render(" ")
	label = ansi.Truncate(label, avail, "")
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
	if tw := ansi.StringWidth(text); tw+2 > limit {
		if limit < minTitleW {
			return 0
		}
		text = "…" + ansi.TruncateLeft(text, tw-(limit-3), "")
	}
	label := lipgloss.NewStyle().Background(t.Surface).Foreground(theme.Mix(t.TextDim, t.Ice, glow)).Render(" " + text + " ")
	lw := ansi.StringWidth(label)
	uv.NewStyledString(label).Draw(buf, uv.Rect(x+w-2-lw, y, lw, 1))
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

// Darkens the whole stage so the overlay stands out
func (m *Model) paintBackdrop(buf screen) {
	if m.theme.Plain {
		return
	}
	shade := m.memo(func(c color.Color) color.Color { return theme.Shade(c, backdropDim) })
	for y := barHeight; y < m.height; y++ {
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

// Draws toasts stacked under the bar at the right, sliding in from the edge
func (m *Model) paintToasts(buf screen) {
	if len(m.toasts) == 0 || m.width < 12 {
		return
	}
	t := m.theme
	limit := max(10, m.width/2)
	for i, tt := range m.toasts {
		y := barHeight + i
		if y >= m.height {
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
		x := m.width - m.gapOutX - lw + int(math.Round(tt.x.Pos*float64(lw+m.gapOutX)))
		uv.NewStyledString(label).Draw(buf, uv.Rect(x, y, lw, 1))
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

// Reuses the cell buffer between frames, reallocating only when the screen changes size
func (m *Model) frame() screen {
	if m.buf.Buffer == nil || m.buf.Width() != m.width || m.buf.Height() != m.height {
		m.buf = screen{uv.NewBuffer(m.width, m.height)}
	}
	return m.buf
}

func (screen) WidthMethod() uv.WidthMethod { return uv.WidthMethod(ansi.GraphemeWidth) }

// Writes the keys of the actions the card answers into its bottom edge, as many as fit, remembering where each sits for clicks
func (m *Model) paintHints(buf screen, e *entry, x, y, w int) {
	e.hints = e.hints[:0]
	hints := append([]Hint(nil), hintsOf(e.card)...)
	if e.ws != nil && closable(e.card) {
		if slices.Contains(contextsOf(e.card), keys.Panel) {
			hints = append(hints, Hint{Action: "panel.close"})
		} else {
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
	col := x + 2 + 1
	used := 1
	for _, h := range hints {
		key, label := m.hintText(h)
		if key == "" {
			continue
		}
		sep := ""
		if len(e.hints) > 0 {
			sep = base.Foreground(t.Steel).Render(" · ")
		}
		text := base.Foreground(t.Ice).Render(key)
		if label != "" {
			text += base.Foreground(t.TextDim).Render(" " + label)
		}
		width := ansi.StringWidth(text)
		if used+ansi.StringWidth(sep)+width+1 > avail {
			break
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
	uv.NewStyledString(sb.String()).Draw(buf, uv.Rect(x+2, y, used+1, 1))
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
