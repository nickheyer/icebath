package wm

import (
	"math"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Color role of a bar segment
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

// One piece of text in the bar
type Segment struct {
	Text  string
	Style SegmentStyle
}

// Chip of one workspace on the bar
type chip struct {
	x, w int
	ws   *Workspace
}

// Clickable parts of the bar beyond the chips
const (
	chipNone  = -1
	chipNew   = -2
	chipClose = -3
)

const (
	newChip   = " + "
	statusGap = 2
)

// Role of one piece of a chip label, each painted in its own weight and color
type chipRole int

const (
	chipNumber chipRole = iota
	chipPad
	chipBusy
	chipClosing
)

// Piece of a chip label with its role
type chipPart struct {
	text string
	role chipRole
}

// Pieces of a workspace chip, its number, busy dot and, on the active one of several, the close mark
func (m *Model) chipParts(i int, ws *Workspace) []chipPart {
	parts := []chipPart{{" " + strconv.Itoa(i+1), chipNumber}}
	if ws.busy {
		parts = append(parts, chipPart{" " + m.theme.Glyphs.Live, chipBusy})
	}
	if i == m.active && len(m.spaces) > 1 {
		parts = append(parts, chipPart{" " + m.theme.Glyphs.Close, chipClosing})
	}
	return append(parts, chipPart{" ", chipPad})
}

// Label of a workspace chip
func (m *Model) chipLabel(i int, ws *Workspace) string {
	var sb strings.Builder
	for _, p := range m.chipParts(i, ws) {
		sb.WriteString(p.text)
	}
	return sb.String()
}

// Lays the chips out from the stage margin and points the highlight at the active one
func (m *Model) retargetChips() {
	m.chips = m.chips[:0]
	x := m.gapOutX
	for i, ws := range m.spaces {
		w := ansi.StringWidth(m.chipLabel(i, ws))
		m.chips = append(m.chips, chip{x: x, w: w, ws: ws})
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

func (m *Model) stepChips(dt float64) bool {
	if m.motion == 0 {
		m.chipX.Snap()
		m.chipW.Snap()
		return false
	}
	a := m.chipX.Step(dt / m.motion)
	b := m.chipW.Step(dt / m.motion)
	return a || b
}

// Reports whether column x lies on the palette button at the right end of the bar
func (m *Model) onMenu(x int) bool {
	return m.menuW > 0 && x >= m.menuX && x < m.menuX+m.menuW
}

// Column right after the last chip, where the new chip starts
func (m *Model) chipsEnd() int {
	if n := len(m.chips); n > 0 {
		return m.chips[n-1].x + m.chips[n-1].w
	}
	return m.gapOutX
}

// Part of the bar under column x beyond a plain chip, the new chip or the close mark of the active one
func (m *Model) chipPartAt(x int) int {
	end := m.chipsEnd()
	if x >= end && x < end+len(newChip) {
		return chipNew
	}
	if i := m.chipAt(x); i == m.active && i >= 0 && len(m.spaces) > 1 && x == m.chips[i].x+m.chips[i].w-2 {
		return chipClose
	}
	return chipNone
}

// Index of the chip under column x, or -1
func (m *Model) chipAt(x int) int {
	for i, c := range m.chips {
		if x >= c.x && x < c.x+c.w {
			return i
		}
	}
	return -1
}

func (m *Model) segmentColor(s SegmentStyle) (fg lipgloss.Style) {
	t := m.theme
	st := lipgloss.NewStyle().Background(t.Bg)
	switch s {
	case Dim:
		return st.Foreground(t.TextDim)
	case Cool:
		return st.Foreground(t.Ice)
	case Warm:
		return st.Foreground(t.Accent)
	case Hot:
		return st.Foreground(t.Pulse)
	case Good:
		return st.Foreground(t.Alive)
	case Bad:
		return st.Foreground(t.Alarm)
	}
	return st.Foreground(t.Text)
}

// Style of one chip piece, lit on the highlight, pulsing when urgent, breathing when busy and raised under the pointer
func (m *Model) chipStyle(i int, ws *Workspace, role chipRole, lit bool) uv.Style {
	t := m.theme
	st := uv.Style{Fg: t.TextDim, Bg: t.Bg}
	switch {
	case lit:
		st = uv.Style{Fg: t.Bg, Bg: t.Ice}
	case ws.urgent:
		beat := 0.55 + 0.45*math.Sin(m.phase*pulseSpeed)
		if m.motion == 0 {
			beat = 1
		}
		st = uv.Style{Fg: theme.Mix(t.TextDim, t.Pulse, beat), Bg: t.Bg, Attrs: uv.AttrBold}
	case i == m.barHot:
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
	case chipBusy:
		breath := 0.55 + 0.45*math.Sin(m.phase*breathSpeed)
		if m.motion == 0 || t.Plain {
			breath = 1
		}
		st.Fg = theme.Mix(t.TextDim, t.Alive, breath)
		if lit {
			st.Fg = t.Bg
		}
	case chipClosing:
		if lit {
			st.Fg = theme.Mix(t.Bg, t.Alarm, 0.6)
			if m.chipHot == chipClose {
				st.Fg = t.Alarm
			}
		}
	}
	return st
}

// Draws the workspace chips with the gliding highlight, the status segments at the right and the palette button
func (m *Model) paintBar(buf screen) {
	t := m.theme
	if m.width == 0 || m.height == 0 {
		return
	}
	m.retargetChips()
	buf.FillArea(&uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: t.Bg}}, uv.Rect(0, 0, m.width, barHeight))
	hx := int(math.Round(m.chipX.Pos))
	hw := int(math.Round(m.chipW.Pos))
	for x := hx; x < hx+hw && x < m.width; x++ {
		if x >= 0 {
			m.put(buf, x, 0, " ", uv.Style{Bg: t.Ice})
		}
	}
	for i, c := range m.chips {
		x := c.x
		for _, p := range m.chipParts(i, c.ws) {
			for _, r := range []rune(p.text) {
				if x >= m.width {
					break
				}
				m.put(buf, x, 0, string(r), m.chipStyle(i, c.ws, p.role, x >= hx && x < hx+hw))
				x++
			}
		}
	}
	newFg, newBg := t.TextDim, t.Bg
	if m.chipHot == chipNew {
		newFg, newBg = t.Bg, t.Accent
	}
	for i, r := range []rune(newChip) {
		m.put(buf, m.chipsEnd()+i, 0, string(r), uv.Style{Fg: newFg, Bg: newBg, Attrs: uv.AttrBold})
	}
	menu := " " + t.Glyphs.Menu + " commands "
	m.menuW = ansi.StringWidth(menu)
	m.menuX = max(m.width-m.gapOutX-m.menuW, 0)
	menuFg, menuBg := t.Accent, t.Bg
	if m.menuHot {
		menuFg, menuBg = t.Bg, t.Accent
	}
	for i, r := range []rune(menu) {
		m.put(buf, m.menuX+i, 0, string(r), uv.Style{Fg: menuFg, Bg: menuBg, Attrs: uv.AttrBold})
	}
	m.paintStatusSegments(buf)
	m.paintPillCaps(buf, hx, hw)
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

// Draws the app segments right aligned before the palette button, cut from the left when chips crowd them
func (m *Model) paintStatusSegments(buf screen) {
	if m.bar == nil {
		return
	}
	t := m.theme
	left, right := m.bar()
	sep := lipgloss.NewStyle().Background(t.Bg).Foreground(t.Steel).Render(" " + t.Glyphs.Sep + " ")
	parts := make([]string, 0, len(left)+len(right))
	for _, s := range append(append([]Segment(nil), left...), right...) {
		if s.Text == "" {
			continue
		}
		parts = append(parts, m.segmentColor(s.Style).Render(s.Text))
	}
	status := strings.Join(parts, sep)
	sw := ansi.StringWidth(status)
	start := m.chipsEnd() + len(newChip) + statusGap
	avail := m.menuX - statusGap - start
	if sw == 0 || avail < minTitleW {
		return
	}
	if sw > avail {
		status = ansi.TruncateLeft(status, sw-avail+ansi.StringWidth(t.Glyphs.Ellipsis), t.Glyphs.Ellipsis)
		sw = ansi.StringWidth(status)
	}
	uv.NewStyledString(status).Draw(buf, uv.Rect(m.menuX-statusGap-sw, 0, sw, 1))
}
