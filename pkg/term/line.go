// Terminal emulator interpreting xterm control sequences into a cell grid with scrollback, driving a program on a pty
package term

import (
	uv "github.com/charmbracelet/ultraviolet"
)

// Shell integration marks a row may carry, set through OSC 133
const (
	MarkPrompt uint8 = 1 << iota
	MarkInput
	MarkOutput
	MarkEnd
)

// Row of cells with whether it continues on the row below and the shell marks it carries
type Line struct {
	Cells   []uv.Cell
	Wrapped bool
	Marks   uint8
	// Exit code of the command that ended on this row when MarkEnd is set
	Exit int
	// Cells protected from selective erase, nil while none are
	guard []bool
}

// Marks or clears the protection of column x
func (l *Line) setGuard(x int, on bool) {
	if l.guard == nil {
		if !on {
			return
		}
		l.guard = make([]bool, len(l.Cells))
	}
	if x >= 0 && x < len(l.guard) {
		l.guard[x] = on
	}
}

// Reports whether column x is protected from selective erase
func (l *Line) guarded(x int) bool {
	return l.guard != nil && x >= 0 && x < len(l.guard) && l.guard[x]
}

// Row of w blank cells
func newLine(w int) Line {
	cells := make([]uv.Cell, w)
	for i := range cells {
		cells[i] = uv.EmptyCell
	}
	return Line{Cells: cells}
}

// Row with its cells copied
func (l Line) clone() Line {
	out := l
	out.Cells = append([]uv.Cell(nil), l.Cells...)
	if l.guard != nil {
		out.guard = append([]bool(nil), l.guard...)
	}
	return out
}

// Blank cell painted with the background of the pen, as erasing leaves it
func blank(pen uv.Style) uv.Cell {
	c := uv.EmptyCell
	if pen.Bg != nil {
		c.Style.Bg = pen.Bg
	}
	return c
}

// Reports whether the cell holds a space with no style or link
func plain(c *uv.Cell) bool {
	return c.Width == 1 && c.Content == " " && c.Style.IsZero() && c.Link.IsZero()
}

// Column after the last cell that is not a plain blank, zero for an empty row
func (l Line) used() int {
	n := len(l.Cells)
	for n > 0 {
		c := &l.Cells[n-1]
		if c.Width == 0 || !plain(c) {
			break
		}
		n--
	}
	return n
}

// Fills columns from through to inclusive with the cell, splitting wide characters cut by the span
func (l Line) fill(from, to int, c uv.Cell) {
	from, to = max(from, 0), min(to, len(l.Cells)-1)
	for x := from; x <= to; x++ {
		uv.Line(l.Cells).Set(x, &c)
		if l.guard != nil {
			l.guard[x] = false
		}
	}
}

// Sets the cell at x, blanking the halves of any wide character it cuts through
func (l Line) set(x int, c uv.Cell) {
	uv.Line(l.Cells).Set(x, &c)
}

// Moves cells from the span starting at from to the span starting at to, both n long, inside the row
func (l Line) move(to, from, n int) {
	copy(l.Cells[to:to+n], l.Cells[from:from+n])
	if l.guard != nil {
		copy(l.guard[to:to+n], l.guard[from:from+n])
	}
}
