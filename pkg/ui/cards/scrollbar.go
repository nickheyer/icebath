package cards

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Scrollbar beside scrolling content, its caps showing the paging keys and its track a timeline of turns and matches
type gutter struct {
	h       int
	total   int
	offset  int
	visible int
	// Lines that start a turn, ticked along the track
	marks []int
	// Lines holding a search match
	hits []int
	// Chords that page up and down
	up   string
	down string
}

// What a click on the gutter asks for
type gutterHit int

const (
	hitNone gutterHit = iota
	hitPageUp
	hitPageDown
	hitJump
)

// First row of the track and its length, between the two caps
func (g gutter) track() (start, length int) {
	return 1, max(g.h-2, 0)
}

// Reports whether the content runs past the rows in view
func (g gutter) scrolls() bool { return g.total > g.visible && g.total > 0 }

// Rows the thumb covers within the track, none while nothing scrolls
func (g gutter) thumb() (start, length int) {
	ts, tl := g.track()
	if tl <= 0 || !g.scrolls() {
		return ts, 0
	}
	length = min(max(g.visible*tl/g.total, 1), tl)
	start = ts + g.offset*(tl-length)/max(g.total-g.visible, 1)
	return start, length
}

// Track row a content line falls on, beside the line itself while nothing scrolls
func (g gutter) rowOf(line int) int {
	ts, tl := g.track()
	if !g.scrolls() {
		return min(max(line, ts), ts+tl-1)
	}
	return ts + line*tl/g.total
}

// Cap of a paging chord, the key's own symbol when it has one and the arrow with the chord as text beside it otherwise
func capOf(chord, arrow string, plain bool) (glyph, label string) {
	if sym, ok := keys.Symbol(chord); ok && !plain {
		return sym, ""
	}
	return arrow, keys.Format(chord)
}

// Column of h cells, one per row
func (g gutter) column(t theme.Theme) []string {
	if g.h <= 0 {
		return nil
	}
	upArrow, downArrow, line, heavy, tick, heavyTick, hit := "▲", "▼", "│", "┃", "┼", "╋", "◆"
	if t.Plain {
		upArrow, downArrow, line, heavy, tick, heavyTick, hit = "^", "v", "|", "#", "+", "+", "*"
	}
	capFg := t.TextDim
	if g.scrolls() {
		capFg = t.Ice
	}
	capStyle := lipgloss.NewStyle().Foreground(capFg).Bold(true)
	out := make([]string, g.h)
	up, _ := capOf(g.up, upArrow, t.Plain)
	down, _ := capOf(g.down, downArrow, t.Plain)
	out[0] = capStyle.Render(up)
	if g.h == 1 {
		return out
	}
	out[g.h-1] = capStyle.Render(down)
	ts, tl := g.track()
	th, thl := g.thumb()
	kinds := make([]int, g.h)
	if tl > 0 && g.total > 0 {
		for _, l := range g.marks {
			if row := g.rowOf(l); row >= ts && row < ts+tl {
				kinds[row] = max(kinds[row], 1)
			}
		}
		for _, l := range g.hits {
			if row := g.rowOf(l); row >= ts && row < ts+tl {
				kinds[row] = 2
			}
		}
	}
	for row := ts; row < ts+tl; row++ {
		onThumb := row >= th && row < th+thl
		st, glyph := lipgloss.NewStyle().Foreground(t.Steel), line
		if onThumb {
			st, glyph = lipgloss.NewStyle().Foreground(t.Ice), heavy
		}
		switch kinds[row] {
		case 1:
			glyph = tick
			if onThumb {
				glyph = heavyTick
			} else {
				st = st.Foreground(t.SteelBright)
			}
		case 2:
			st, glyph = lipgloss.NewStyle().Foreground(t.Accent).Bold(true), hit
		}
		out[row] = st.Render(glyph)
	}
	return out
}

// Meaning of a click on row y, the line to show first for a jump
func (g gutter) hit(y int) (gutterHit, int) {
	if y < 0 || y >= g.h {
		return hitNone, 0
	}
	ts, tl := g.track()
	switch {
	case y == 0:
		return hitPageUp, 0
	case y == g.h-1:
		return hitPageDown, 0
	case tl <= 0 || !g.scrolls():
		return hitNone, 0
	}
	line := (y - ts) * g.total / tl
	return hitJump, min(max(line-g.visible/2, 0), max(g.total-g.visible, 0))
}

// Body fitted to two columns short of w with the gutter in the last column, a chord without a symbol set beside its cap
func (g gutter) beside(body string, w int, t theme.Theme) string {
	col := g.column(t)
	rows := strings.Split(body, "\n")
	for len(rows) < g.h {
		rows = append(rows, "")
	}
	_, upLabel := capOf(g.up, "", t.Plain)
	_, downLabel := capOf(g.down, "", t.Plain)
	label := lipgloss.NewStyle().Foreground(t.TextDim)
	for i := range rows[:g.h] {
		text := ""
		switch {
		case i == 0:
			text = upLabel
		case i == g.h-1:
			text = downLabel
		}
		if text == "" || ansi.StringWidth(text)+4 > w {
			rows[i] = render.Fit(rows[i], w-2) + " " + col[i]
			continue
		}
		lw := ansi.StringWidth(text)
		rows[i] = render.Fit(rows[i], w-3-lw) + " " + label.Render(text) + " " + col[i]
	}
	return strings.Join(rows[:g.h], "\n")
}
