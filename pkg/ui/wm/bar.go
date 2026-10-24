package wm

import (
	"image/color"
	"math"
	"strconv"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// What a workspace or a waiting item reports, later signals needing the person more
type Signal int

const (
	Idle Signal = iota
	Working
	Fresh
	Failed
	Blocked
)

// Live state of one workspace as its chip shows it
type Lane struct {
	Name   string
	Signal Signal
	// Text after the signal mark, such as the tool running and how long the run has taken
	Detail string
}

// Item waiting on the person, shown in the field until its action serves it
type Notice struct {
	Text   string
	Signal Signal
	// Items queued behind this one
	More   int
	Action string
}

// Color role of a dial
type SegmentStyle int

const (
	Plain SegmentStyle = iota
	Dim
	Cool
	Warm
	Hot
	Good
	Bad
)

// Control at the right of the bar that fires its action when clicked
type Segment struct {
	Text   string
	Style  SegmentStyle
	Action string
}

// Everything the app shows on the bar for one frame
type Status struct {
	Lanes  map[*Workspace]Lane
	Notice Notice
	// Least important first, dropped from the left when the bar runs out of room
	Dials []Segment
}

// Chip of one workspace on the bar with the pieces of its label
type chip struct {
	x, w  int
	ws    *Workspace
	lane  Lane
	parts []chipPart
}

// Dial placed on the bar
type dial struct {
	x, w int
	seg  Segment
}

// Kind of bar part under the pointer
type barKind int

const (
	barNone barKind = iota
	barChip
	barNew
	barClose
	barField
	barDial
)

// Bar part under the pointer with the index of its chip or dial
type barSpot struct {
	kind barKind
	i    int
}

const (
	newChip = " + "
	// Columns kept clear between the chips, the field and the dials
	barGap = 2
	// Narrowest the field shrinks to before it hides
	fieldMin = 8
	// Width the field keeps while chips and dials still have room to give
	fieldEase = 40
	// Widest the field grows while it shows its own text
	fieldMax = 56
	// Most rows the sheet under the field lists
	sheetRows = 14
	// Widest the detail after a lane mark runs
	detailCols = 16
	// Spinner frames per unit of flow phase
	spinRate = 0.5
)

// Name columns a chip keeps at each detail level, the last level dropping the name and the detail
var nameCols = [...]int{18, 8, 0}

// Role of one piece of a chip label, each painted in its own weight and color
type chipRole int

const (
	chipNumber chipRole = iota
	chipName
	chipMark
	chipDetail
	chipClosing
	chipPad
)

// Piece of a chip label with its role
type chipPart struct {
	text string
	role chipRole
}

// Number, name, signal mark and detail of a lane at a level, the active of several adding a close mark
func (m *Model) chipParts(i int, l Lane, level int) []chipPart {
	g := m.theme.Glyphs
	parts := []chipPart{{" " + strconv.Itoa(i+1), chipNumber}}
	if name := strings.Join(strings.Fields(l.Name), " "); nameCols[level] > 0 && name != "" {
		parts = append(parts, chipPart{" " + ansi.Truncate(name, nameCols[level], g.Ellipsis), chipName})
	}
	if mark := m.signalMark(l.Signal); mark != "" {
		parts = append(parts, chipPart{" " + mark, chipMark})
	}
	if detail := strings.Join(strings.Fields(l.Detail), " "); level == 0 && detail != "" {
		parts = append(parts, chipPart{" " + ansi.Truncate(detail, detailCols, g.Ellipsis), chipDetail})
	}
	if i == m.active && len(m.spaces) > 1 {
		parts = append(parts, chipPart{" " + g.Close, chipClosing})
	}
	return append(parts, chipPart{" ", chipPad})
}

func partsWidth(parts []chipPart) int {
	w := 0
	for _, p := range parts {
		w += ansi.StringWidth(p.text)
	}
	return w
}

// Mark of a signal, the spinner frame of the moment while working and nothing while idle
func (m *Model) signalMark(s Signal) string {
	g := m.theme.Glyphs
	switch s {
	case Working:
		return g.Spinner[int(m.phase*spinRate)%len(g.Spinner)]
	case Fresh:
		return g.Check
	case Failed:
		return g.Fail
	case Blocked:
		return g.Flag
	}
	return ""
}

// Color a signal is painted in, a waiting approval beating with the urgency pulse
func (m *Model) signalColor(s Signal) color.Color {
	t := m.theme
	switch s {
	case Working:
		return t.Alive
	case Fresh:
		return t.Ice
	case Failed:
		return t.Alarm
	case Blocked:
		return theme.Mix(t.TextDim, t.Pulse, m.beat())
	}
	return t.TextDim
}

// Status the app reports for this frame, empty while it reports none
func (m *Model) barStatus() Status {
	if m.report == nil {
		return Status{}
	}
	return m.report()
}

// Fits chips, field and dials on the row, the field narrowing first and the mode dial leaving last
func (m *Model) layoutBar() {
	st := m.barStatus()
	m.notice = st.Notice
	var dials []Segment
	for _, d := range st.Dials {
		if d.Text != "" {
			dials = append(dials, d)
		}
	}
	want := m.fieldWant()
	floor, level, fw := min(want, fieldEase), 0, 0
fit:
	for {
		need := m.barNeed(level, st.Lanes, dials)
		if rest := m.width - need - barGap - 2; floor > 0 && rest >= floor {
			fw = min(want, rest)
			break
		}
		if floor == 0 && need <= m.width {
			break
		}
		switch {
		case level == 0:
			level = 1
		case len(dials) > 1:
			dials = dials[1:]
		case level == 1:
			level = 2
		case floor > fieldMin:
			floor = fieldMin
		case floor > 0:
			floor = 0
		case len(dials) > 0:
			dials = nil
		default:
			break fit
		}
	}
	m.placeChips(level, st.Lanes)
	m.placeDials(dials)
	m.placeField(fw)
}

// Columns the chips at a level and the dials take with the margins and the new chip
func (m *Model) barNeed(level int, lanes map[*Workspace]Lane, dials []Segment) int {
	need := 2*m.gapOutX + len(newChip)
	for i, ws := range m.spaces {
		need += partsWidth(m.chipParts(i, lanes[ws], level))
	}
	if dw := m.dialsWidth(dials); dw > 0 {
		need += barGap + dw
	}
	return need
}

// Lays the chips out from the stage margin and points the highlight at the active one
func (m *Model) placeChips(level int, lanes map[*Workspace]Lane) {
	m.chips = m.chips[:0]
	x := m.gapOutX
	for i, ws := range m.spaces {
		l := lanes[ws]
		parts := m.chipParts(i, l, level)
		w := partsWidth(parts)
		m.chips = append(m.chips, chip{x: x, w: w, ws: ws, lane: l, parts: parts})
		if i == m.active {
			m.chipX.Set(float64(x))
			m.chipW.Set(float64(w))
		}
		x += w
	}
	if m.motion == 0 {
		m.chipX.Snap()
		m.chipW.Snap()
	}
}

// Text between two dials
func (m *Model) dialSep() string { return " " + m.theme.Glyphs.Sep + " " }

func (m *Model) dialsWidth(segs []Segment) int {
	w := 0
	for i, s := range segs {
		if i > 0 {
			w += ansi.StringWidth(m.dialSep())
		}
		w += ansi.StringWidth(s.Text)
	}
	return w
}

// Lays the dials out so the last ends at the stage margin
func (m *Model) placeDials(segs []Segment) {
	m.dials = m.dials[:0]
	sep := ansi.StringWidth(m.dialSep())
	x := m.width - m.gapOutX - m.dialsWidth(segs)
	for i, s := range segs {
		if i > 0 {
			x += sep
		}
		w := ansi.StringWidth(s.Text)
		m.dials = append(m.dials, dial{x: x, w: w, seg: s})
		x += w
	}
}

// Centers the field between chips and dials, or wide on the whole row while a palette types into it
func (m *Model) placeField(fw int) {
	x, w := m.width/2, fw
	switch {
	case m.docked() != nil:
		x, w = m.openField()
	case fw > 0:
		lo := m.chipsEnd() + len(newChip) + barGap + 1
		hi := m.width - m.gapOutX - 1 - fw
		if len(m.dials) > 0 {
			hi = m.dials[0].x - barGap - 1 - fw
		}
		x = min(max((m.width-fw)/2, lo), max(hi, lo))
	}
	m.fieldX.Set(float64(x))
	m.fieldW.Set(float64(w))
	if m.motion == 0 {
		m.fieldX.Snap()
		m.fieldW.Snap()
	}
}

// Box of the field while a palette types into it, three fifths of the bar centered on it
func (m *Model) openField() (int, int) {
	w := max(min(max(m.width*3/5, 44), m.width-2*m.gapOutX-2), 1)
	return (m.width - w) / 2, w
}

// Column and width the field is painted at this frame, between its caps
func (m *Model) fieldBox() (int, int) {
	return int(math.Round(m.fieldX.Pos)), int(math.Round(m.fieldW.Pos))
}

// Palette open as the overlay, which types into the field, nil while none is
func (m *Model) docked() *Palette {
	if m.overlay == nil {
		return nil
	}
	p, _ := m.overlay.card.(*Palette)
	return p
}

// Mark, words, queued count and key a closed field shows
type fieldText struct {
	mark  string
	tint  color.Color
	words string
	more  string
	key   string
}

// Waiting item the app raised, else the palette with the key that opens it
func (m *Model) closedField() fieldText {
	t := m.theme
	if n := m.notice; n.Text != "" {
		ft := fieldText{mark: m.signalMark(n.Signal), tint: m.signalColor(n.Signal), words: strings.Join(strings.Fields(n.Text), " "), key: m.keys.Display(n.Action)}
		if n.More > 0 {
			ft.more = "+" + strconv.Itoa(n.More)
		}
		return ft
	}
	words := "palette"
	if k := m.keys.Display("palette"); k != "" {
		words += " (" + k + ")"
	}
	return fieldText{mark: t.Glyphs.Prompt, tint: t.TextDim, words: words}
}

// Columns the closed field wants for its text and key, within its bounds
func (m *Model) fieldWant() int {
	ft := m.closedField()
	w := 1 + ansi.StringWidth(ft.mark) + 1 + ansi.StringWidth(ft.words) + 1
	if ft.more != "" {
		w += 1 + ansi.StringWidth(ft.more)
	}
	if ft.key != "" {
		w += 2 + ansi.StringWidth(ft.key)
	}
	return min(max(w, fieldMin), fieldMax)
}

// Bar part under column x, the field first since it paints over the rest while open
func (m *Model) barAt(x int) barSpot {
	if fx, fw := m.fieldBox(); fw >= 3 && x >= fx-1 && x <= fx+fw {
		return barSpot{kind: barField}
	}
	for i, d := range m.dials {
		if x >= d.x && x < d.x+d.w {
			return barSpot{kind: barDial, i: i}
		}
	}
	if end := m.chipsEnd(); x >= end && x < end+len(newChip) {
		return barSpot{kind: barNew}
	}
	for i, c := range m.chips {
		if x < c.x || x >= c.x+c.w {
			continue
		}
		if i == m.active && len(m.spaces) > 1 && x == c.x+c.w-2 {
			return barSpot{kind: barClose, i: i}
		}
		return barSpot{kind: barChip, i: i}
	}
	return barSpot{}
}

// Column right after the last chip, where the new chip starts
func (m *Model) chipsEnd() int {
	if n := len(m.chips); n > 0 {
		return m.chips[n-1].x + m.chips[n-1].w
	}
	return m.gapOutX
}

func (m *Model) stepBar(dt float64) bool {
	springs := []*anim.Value{&m.chipX, &m.chipW, &m.fieldX, &m.fieldW}
	moving := false
	for _, v := range springs {
		if m.motion == 0 {
			v.Snap()
			continue
		}
		moving = v.Step(dt/m.motion) || moving
	}
	return moving
}

// Foreground of a dial style
func (m *Model) styleColor(s SegmentStyle) color.Color {
	t := m.theme
	switch s {
	case Dim:
		return t.TextDim
	case Cool:
		return t.Ice
	case Warm:
		return t.Accent
	case Hot:
		return t.Pulse
	case Good:
		return t.Alive
	case Bad:
		return t.Alarm
	}
	return t.Text
}

// Style of one chip piece, lit on the highlight, pulsing when urgent and raised under the pointer
func (m *Model) chipStyle(i int, c chip, role chipRole, lit bool) uv.Style {
	t := m.theme
	ws := c.ws
	hot := m.hot == barSpot{kind: barChip, i: i}
	st := uv.Style{Fg: t.TextDim, Bg: t.Bg}
	switch {
	case lit:
		st = uv.Style{Fg: t.Bg, Bg: t.Ice}
	case ws.urgent:
		st = uv.Style{Fg: theme.Mix(t.TextDim, t.Pulse, m.beat()), Bg: t.Bg, Attrs: uv.AttrBold}
	case hot:
		st.Fg = t.Text
		if !t.Plain {
			st.Bg = theme.Mix(t.Bg, t.Steel, 0.4)
		}
	}
	switch role {
	case chipNumber:
		st.Attrs |= uv.AttrBold
		if !lit && !ws.urgent {
			st.Fg = t.Text
		}
	case chipName:
		if !lit && !ws.urgent && !hot {
			st.Fg = theme.Mix(t.TextDim, t.Text, 0.5)
		}
	case chipMark:
		st.Attrs |= uv.AttrBold
		st.Fg = m.signalColor(c.lane.Signal)
		if lit {
			st.Fg = t.Bg
		}
	case chipDetail:
		st.Attrs &^= uv.AttrBold
		if lit {
			st.Fg = theme.Mix(t.Bg, t.Ice, 0.35)
		}
	case chipClosing:
		if lit {
			st.Fg = theme.Mix(t.Bg, t.Alarm, 0.6)
			if m.hot.kind == barClose {
				st.Fg = t.Alarm
			}
		}
	}
	return st
}

// Writes text one grapheme per cell from column x styled by column and returns the column after it
func (m *Model) write(buf screen, x, y int, s string, style func(x int) uv.Style) int {
	for s != "" && x < m.width {
		g, w := ansi.FirstGraphemeCluster(s, ansi.GraphemeWidth)
		if g == "" {
			break
		}
		s = s[len(g):]
		if w == 0 {
			continue
		}
		m.put(buf, x, y, g, style(x))
		x += w
	}
	return x
}

// One style for every cell
func flat(st uv.Style) func(int) uv.Style { return func(int) uv.Style { return st } }

// Draws the lane chips with the gliding highlight, the dials at the right, then the field over them
func (m *Model) paintBar(buf screen) {
	t := m.theme
	if m.width == 0 || m.height == 0 {
		return
	}
	m.layoutBar()
	buf.FillArea(&uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: t.Bg}}, uv.Rect(0, 0, m.width, barHeight))
	hx := int(math.Round(m.chipX.Pos))
	hw := int(math.Round(m.chipW.Pos))
	for x := max(hx, 0); x < hx+hw && x < m.width; x++ {
		m.put(buf, x, 0, " ", uv.Style{Bg: t.Ice})
	}
	for i, c := range m.chips {
		x := c.x
		for _, p := range c.parts {
			x = m.write(buf, x, 0, p.text, func(cx int) uv.Style { return m.chipStyle(i, c, p.role, cx >= hx && cx < hx+hw) })
		}
	}
	newFg, newBg := t.TextDim, t.Bg
	if m.hot.kind == barNew {
		newFg, newBg = t.Bg, t.Accent
	}
	m.write(buf, m.chipsEnd(), 0, newChip, flat(uv.Style{Fg: newFg, Bg: newBg, Attrs: uv.AttrBold}))
	m.paintDials(buf)
	m.paintPillCaps(buf, hx, hw)
	m.paintField(buf)
}

// Draws the dials with separators, raising the one under the pointer when it has an action
func (m *Model) paintDials(buf screen) {
	t := m.theme
	sep := m.dialSep()
	sw := ansi.StringWidth(sep)
	for i, d := range m.dials {
		if i > 0 {
			m.write(buf, d.x-sw, 0, sep, flat(uv.Style{Fg: t.Steel, Bg: t.Bg}))
		}
		st := uv.Style{Fg: m.styleColor(d.seg.Style), Bg: t.Bg}
		if m.hot == (barSpot{kind: barDial, i: i}) && d.seg.Action != "" {
			st.Attrs |= uv.AttrBold
			if !t.Plain {
				st.Bg = theme.Mix(t.Bg, t.Steel, 0.4)
			}
		}
		m.write(buf, d.x, 0, d.seg.Text, flat(st))
	}
}

// Rounds the ends of the highlight with half cells where the bar is blank beside it
func (m *Model) paintPillCaps(buf screen, hx, hw int) {
	t := m.theme
	if t.Plain || hw <= 0 {
		return
	}
	for _, cap := range []struct {
		x  int
		ch string
	}{{hx - 1, t.Glyphs.CapLeft}, {hx + hw, t.Glyphs.CapRight}} {
		if cap.x < 0 || cap.x >= m.width {
			continue
		}
		if c := buf.CellAt(cap.x, 0); c == nil || c.Content != " " || c.Style.Bg != t.Bg {
			continue
		}
		m.put(buf, cap.x, 0, cap.ch, uv.Style{Fg: t.Ice, Bg: t.Bg})
	}
}

// Surface of the field, tinted by a waiting item and raised under the pointer while closed
func (m *Model) fieldBg() color.Color {
	t := m.theme
	bg := t.Surface
	if t.Plain || m.docked() != nil {
		return bg
	}
	if n := m.notice; n.Text != "" {
		amount := 0.16
		if n.Signal == Blocked {
			amount = 0.08 + 0.14*m.beat()
		}
		bg = theme.Mix(t.Surface, m.signalColor(n.Signal), amount)
	}
	if m.hot.kind == barField {
		bg = theme.Mix(bg, t.Steel, 0.45)
	}
	return bg
}

// Draws the field as a capped pill holding the palette input, a waiting item or the way into the commands
func (m *Model) paintField(buf screen) {
	m.caret = -1
	fx, fw := m.fieldBox()
	if fw < 3 {
		return
	}
	t := m.theme
	bg := m.fieldBg()
	for x := max(fx, 0); x < fx+fw && x < m.width; x++ {
		m.put(buf, x, 0, " ", uv.Style{Bg: bg})
	}
	if !t.Plain {
		m.put(buf, fx-1, 0, t.Glyphs.CapLeft, uv.Style{Fg: bg, Bg: t.Bg})
		if fx+fw < m.width {
			m.put(buf, fx+fw, 0, t.Glyphs.CapRight, uv.Style{Fg: bg, Bg: t.Bg})
		}
	}
	left, right := fx+1, min(fx+fw-1, m.width)
	if p := m.docked(); p != nil {
		m.paintInput(buf, p, left, right, bg)
		return
	}
	ft := m.closedField()
	if kw := ansi.StringWidth(ft.key); kw > 0 && right-left >= kw+2+fieldMin {
		m.write(buf, right-kw, 0, ft.key, flat(uv.Style{Fg: ft.tint, Bg: bg}))
		right -= kw + 2
	}
	x := m.write(buf, left, 0, ft.mark, flat(uv.Style{Fg: ft.tint, Bg: bg, Attrs: uv.AttrBold})) + 1
	wordsFg := t.Text
	if m.notice.Text == "" {
		wordsFg = t.TextDim
	}
	more := 0
	if ft.more != "" {
		more = 1 + ansi.StringWidth(ft.more)
	}
	x = m.write(buf, x, 0, ansi.Truncate(ft.words, max(right-x-more, 0), t.Glyphs.Ellipsis), flat(uv.Style{Fg: wordsFg, Bg: bg}))
	if more > 0 && x+more <= right {
		m.write(buf, x+1, 0, ft.more, flat(uv.Style{Fg: t.TextDim, Bg: bg}))
	}
}

// Draws the query of the open palette with its match count or prompt keys and places the caret
func (m *Model) paintInput(buf screen, p *Palette, left, right int, bg color.Color) {
	t := m.theme
	x := m.write(buf, left, 0, t.Glyphs.Prompt, flat(uv.Style{Fg: t.Accent, Bg: bg})) + 1
	tail := m.inputTail(p, bg)
	if tw := spansWidth(tail); tw > 0 && right-x >= tw+2+fieldMin {
		tx := right - tw
		for _, s := range tail {
			tx = m.write(buf, tx, 0, s.text, flat(s.st))
		}
		right -= tw + 2
	}
	if right <= x {
		return
	}
	text, caret := p.Input(right-x, bg)
	uv.NewStyledString(text).Draw(buf, uv.Rect(x, 0, ansi.StringWidth(text), 1))
	m.caret = x + caret
}

// Piece of text with its style
type span struct {
	text string
	st   uv.Style
}

func spansWidth(ss []span) int {
	w := 0
	for _, s := range ss {
		w += ansi.StringWidth(s.text)
	}
	return w
}

// Text at the right of the open field, the match count of a picker or the keys a prompt answers
func (m *Model) inputTail(p *Palette, bg color.Color) []span {
	t := m.theme
	if len(p.items) > 0 {
		return []span{{p.Tally(), uv.Style{Fg: t.TextDim, Bg: bg}}}
	}
	var out []span
	for _, h := range p.Hints() {
		key, label := m.hintText(h)
		if key == "" {
			continue
		}
		if len(out) > 0 {
			out = append(out, span{m.dialSep(), uv.Style{Fg: t.Steel, Bg: bg}})
		}
		out = append(out, span{key, uv.Style{Fg: t.Ice, Bg: bg}})
		if label != "" {
			out = append(out, span{" " + label, uv.Style{Fg: t.TextDim, Bg: bg}})
		}
	}
	return out
}
