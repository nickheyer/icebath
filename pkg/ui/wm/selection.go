package wm

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Sweep of text inside one card's content, in content coordinates
type selection struct {
	e              *entry
	x0, y0, x1, y1 int
	moved          bool
}

// Normalized corners with the start before the end in reading order
func (s *selection) span() (x0, y0, x1, y1 int) {
	x0, y0, x1, y1 = s.x0, s.y0, s.x1, s.y1
	if y1 < y0 || (y1 == y0 && x1 < x0) {
		x0, y0, x1, y1 = x1, y1, x0, y0
	}
	return x0, y0, x1, y1
}

// Reports whether a content cell falls inside the sweep, reading order like a terminal selection
func (s *selection) covers(x, y int) bool {
	x0, y0, x1, y1 := s.span()
	switch {
	case y < y0 || y > y1:
		return false
	case y0 == y1:
		return x >= x0 && x <= x1
	case y == y0:
		return x >= x0
	case y == y1:
		return x <= x1
	}
	return true
}

// Plain text under the sweep, one line per row with trailing blanks trimmed
func (s *selection) text() string {
	raw := s.e.raw
	if raw.Buffer == nil {
		return ""
	}
	x0, y0, x1, y1 := s.span()
	var lines []string
	for y := max(y0, 0); y <= y1 && y < raw.Height(); y++ {
		from, to := 0, raw.Width()-1
		if y == y0 {
			from = max(x0, 0)
		}
		if y == y1 {
			to = min(x1, raw.Width()-1)
		}
		var sb strings.Builder
		for x := from; x <= to; x++ {
			c := raw.CellAt(x, y)
			if c == nil || c.Width == 0 {
				continue
			}
			sb.WriteString(c.Content)
		}
		lines = append(lines, strings.TrimRight(sb.String(), " "))
	}
	return strings.Join(lines, "\n")
}

// Starts a sweep in the entry at the stage point when the card allows selection
func (m *Model) beginSelection(e *entry, x, y int) {
	if s, ok := e.card.(Selectable); !ok || !s.Selectable() {
		m.sel = nil
		return
	}
	c := m.contentRect(e.frame())
	m.sel = &selection{e: e, x0: x - c.X, y0: y - c.Y}
	m.sel.x1, m.sel.y1 = m.sel.x0, m.sel.y0
}

// Extends the sweep to the stage point, clamped to the card content
func (m *Model) extendSelection(x, y int) bool {
	s := m.sel
	if s == nil {
		return false
	}
	c := m.contentRect(s.e.frame())
	nx := min(max(x-c.X, 0), max(c.W-1, 0))
	ny := min(max(y-c.Y, 0), max(c.H-1, 0))
	if nx == s.x1 && ny == s.y1 && s.moved {
		return false
	}
	if nx != s.x0 || ny != s.y0 {
		s.moved = true
	}
	s.x1, s.y1 = nx, ny
	return true
}

// Ends the sweep, copying its text to the clipboard when it covered anything
func (m *Model) endSelection() tea.Cmd {
	s := m.sel
	if s == nil || !s.moved {
		m.sel = nil
		return nil
	}
	text := s.text()
	m.sel = nil
	if strings.TrimSpace(text) == "" {
		return nil
	}
	n := strings.Count(text, "\n") + 1
	label := "copied 1 line"
	if n > 1 {
		label = "copied " + strconv.Itoa(n) + " lines"
	}
	return tea.Batch(tea.SetClipboard(text), m.Toast(label, Success, 0))
}

// Text currently swept, empty when nothing is selected
func (m *Model) Selected() string {
	if m.sel == nil || !m.sel.moved {
		return ""
	}
	return m.sel.text()
}
