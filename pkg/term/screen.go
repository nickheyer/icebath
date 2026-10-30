package term

import (
	uv "github.com/charmbracelet/ultraviolet"
)

// Character set a G designator maps printable bytes through
type charset byte

const (
	charsetASCII    charset = 'B'
	charsetGraphics charset = '0'
	charsetUK       charset = 'A'
)

// Where the next character lands and how it is painted
type cursor struct {
	x, y int
	pen  uv.Style
	link uv.Link
	// A character was written in the last column, the next one wrapping first
	wrap bool
	g    [4]charset
	gl   int
	gr   int
	// Single shift to G2 or G3 for the next character alone, zero when none
	ss     int
	origin bool
}

// One of the two cell grids, the main one feeding the scrollback and the alternate one standing alone
type screen struct {
	lines []Line
	cur   cursor
	saved cursor
	kept  bool
	// Scroll region rows, inclusive
	top, bot int
	// Left and right margins, inclusive, in force while margins mode is on
	left, right int
	tabs        []bool
	// Kitty keyboard flags pushed by programs, the top in force
	kitty []int
	// Styles pushed by XTPUSHSGR
	pens []uv.Style
}

func newScreen(w, h int) *screen {
	s := &screen{}
	s.reset(w, h)
	return s
}

// Returns the grid to a blank w by h with defaults for every setting
func (s *screen) reset(w, h int) {
	s.lines = make([]Line, h)
	for i := range s.lines {
		s.lines[i] = newLine(w)
	}
	s.cur = cursor{g: [4]charset{charsetASCII, charsetASCII, charsetASCII, charsetASCII}, gr: 2}
	s.saved, s.kept = s.cur, false
	s.top, s.bot = 0, h-1
	s.left, s.right = 0, w-1
	s.tabs = defaultTabs(w)
	s.kitty = nil
	s.pens = nil
}

// Tab stops every eight columns
func defaultTabs(w int) []bool {
	tabs := make([]bool, max(w, 0))
	for i := 8; i < w; i += 8 {
		tabs[i] = true
	}
	return tabs
}

func (s *screen) width() int {
	if len(s.lines) == 0 {
		return 0
	}
	return len(s.lines[0].Cells)
}

// Row y, the last row for anything past the end
func (s *screen) line(y int) *Line {
	return &s.lines[min(max(y, 0), len(s.lines)-1)]
}

// Resizes the grid without reflowing, clipping or padding rows and clamping the cursor and the regions
func (s *screen) resize(w, h int) {
	ow := s.width()
	for i := range s.lines {
		l := &s.lines[i]
		if w < ow {
			l.Cells = l.Cells[:w]
			if w > 0 && l.Cells[w-1].Width == 2 {
				l.Cells[w-1] = uv.EmptyCell
			}
		} else {
			for len(l.Cells) < w {
				l.Cells = append(l.Cells, uv.EmptyCell)
			}
		}
	}
	for len(s.lines) < h {
		s.lines = append(s.lines, newLine(w))
	}
	if len(s.lines) > h {
		over := len(s.lines) - h
		if s.cur.y >= h {
			s.lines = s.lines[over:]
			s.cur.y -= over
		} else {
			s.lines = s.lines[:h]
		}
	}
	s.fit(w, h)
}

// Clamps the cursor, the saved cursor, the regions and the tabs to a grid of w by h
func (s *screen) fit(w, h int) {
	s.top, s.bot = 0, h-1
	s.left, s.right = 0, w-1
	s.cur.x, s.cur.y = min(max(s.cur.x, 0), max(w-1, 0)), min(max(s.cur.y, 0), max(h-1, 0))
	s.saved.x, s.saved.y = min(max(s.saved.x, 0), max(w-1, 0)), min(max(s.saved.y, 0), max(h-1, 0))
	s.cur.wrap = false
	tabs := make([]bool, w)
	copy(tabs, s.tabs)
	for i := len(s.tabs); i < w; i++ {
		tabs[i] = i%8 == 0
	}
	s.tabs = tabs
}

// Moves rows top through bot up by n inside the column span, blank rows filling the bottom
func (s *screen) scrollUp(top, bot, left, right, n int, fill uv.Cell) {
	if n <= 0 || top > bot {
		return
	}
	n = min(n, bot-top+1)
	full := left == 0 && right == s.width()-1
	for y := top; y <= bot; y++ {
		if y+n <= bot {
			if full {
				s.lines[y], s.lines[y+n] = s.lines[y+n], s.lines[y]
				continue
			}
			copyCells(&s.lines[y], &s.lines[y+n], left, right)
			continue
		}
		if full {
			s.lines[y] = Line{Cells: s.lines[y].Cells}
		}
		s.lines[y].fill(left, right, fill)
	}
}

// Moves rows top through bot down by n inside the column span, blank rows filling the top
func (s *screen) scrollDown(top, bot, left, right, n int, fill uv.Cell) {
	if n <= 0 || top > bot {
		return
	}
	n = min(n, bot-top+1)
	full := left == 0 && right == s.width()-1
	for y := bot; y >= top; y-- {
		if y-n >= top {
			if full {
				s.lines[y], s.lines[y-n] = s.lines[y-n], s.lines[y]
				continue
			}
			copyCells(&s.lines[y], &s.lines[y-n], left, right)
			continue
		}
		if full {
			s.lines[y] = Line{Cells: s.lines[y].Cells}
		}
		s.lines[y].fill(left, right, fill)
	}
}

// Copies the columns left through right from one row to another with their protection
func copyCells(dst, src *Line, left, right int) {
	copy(dst.Cells[left:right+1], src.Cells[left:right+1])
	if src.guard == nil && dst.guard == nil {
		return
	}
	for x := left; x <= right; x++ {
		dst.setGuard(x, src.guarded(x))
	}
}

// Next tab stop after x, the right margin when none follows
func (s *screen) nextTab(x int) int {
	for i := x + 1; i <= s.right && i < len(s.tabs); i++ {
		if s.tabs[i] {
			return i
		}
	}
	return s.right
}

// Previous tab stop before x, the left margin when none precedes
func (s *screen) prevTab(x int) int {
	for i := x - 1; i > s.left && i < len(s.tabs); i-- {
		if s.tabs[i] {
			return i
		}
	}
	return s.left
}

// Blanks every row of the grid, leaving wraps and marks behind
func (s *screen) clear(fill uv.Cell) {
	for y := range s.lines {
		s.lines[y] = Line{Cells: s.lines[y].Cells}
		s.lines[y].fill(0, s.width()-1, fill)
	}
}
