package term

import (
	uv "github.com/charmbracelet/ultraviolet"
)

// Resizes the main screen to w by h, rewrapping its rows and the scrollback when the width changed
func (e *Emulator) reflow(w, h int) {
	if w == e.w {
		e.resizeHeight(h)
		return
	}
	s := e.main
	rows := append(e.back.all(), s.lines...)
	curRow := e.back.n + s.cur.y
	curX := s.cur.x
	if s.cur.wrap {
		curX++
	}
	var out []Line
	newCur, newX := -1, 0
	for i := 0; i < len(rows); {
		cells, guard, curOff, next := joinRows(rows, i, curRow, curX)
		var at, x int
		out, at, x = wrapCells(out, cells, guard, w, rows[i].Marks, rows[i].Exit, curOff)
		if curOff >= 0 {
			newCur, newX = at, x
		}
		i = next
	}
	if newCur < 0 {
		newCur, newX = max(len(out)-1, 0), 0
	}
	last := len(out) - 1
	for last > newCur && out[last].used() == 0 && out[last].Marks == 0 {
		last--
	}
	out = out[:last+1]
	backCount := max(len(out)-h, 0)
	backCount = min(backCount, newCur)
	screen := append([]Line(nil), out[backCount:]...)
	if len(screen) > h {
		screen = screen[:h]
	}
	for len(screen) < h {
		screen = append(screen, newLine(w))
	}
	e.first += uint64(e.back.replace(out[:backCount]))
	s.lines = screen
	s.cur.y, s.cur.x = newCur-backCount, min(newX, w-1)
	s.fit(w, h)
}

// Cells of the logical row starting at physical row i with their protection, the cursor offset in it or -1,
// and the index of the row after it
func joinRows(rows []Line, i, curRow, curX int) ([]uv.Cell, []bool, int, int) {
	var cells []uv.Cell
	var guard []bool
	curOff := -1
	j := i
	for {
		r := rows[j]
		n := len(r.Cells)
		more := r.Wrapped && j+1 < len(rows)
		switch {
		case !more:
			n = r.used()
		case n > 0 && plain(&r.Cells[n-1]) && len(rows[j+1].Cells) > 0 && rows[j+1].Cells[0].Width == 2:
			n--
		}
		if j == curRow {
			curOff = len(cells) + min(curX, len(r.Cells))
		}
		if r.guard != nil && guard == nil {
			guard = make([]bool, len(cells))
		}
		cells = append(cells, r.Cells[:n]...)
		if guard != nil {
			for k := 0; k < n; k++ {
				guard = append(guard, r.guarded(k))
			}
		}
		if !more {
			return cells, guard, curOff, j + 1
		}
		j++
	}
}

// Appends the cells wrapped into rows of w to out, reporting the row and column the cursor offset landed on
func wrapCells(out []Line, cells []uv.Cell, guard []bool, w int, marks uint8, exit, curOff int) ([]Line, int, int) {
	row := newLine(w)
	row.Marks, row.Exit = marks, exit
	x := 0
	curAt, curX := len(out), 0
	placed := curOff < 0
	emit := func(wrapped bool) {
		row.Wrapped = wrapped
		out = append(out, row)
		row = newLine(w)
		x = 0
	}
	for k := 0; k < len(cells); k++ {
		c := cells[k]
		if c.Width == 0 {
			continue
		}
		if x+c.Width > w {
			emit(true)
		}
		if !placed && k >= curOff {
			curAt, curX, placed = len(out), x, true
		}
		row.Cells[x] = c
		if c.Width == 2 && x+1 < w {
			row.Cells[x+1] = uv.Cell{}
		}
		if guard != nil && k < len(guard) && guard[k] {
			row.setGuard(x, true)
		}
		x += c.Width
		if x >= w && k+1 < len(cells) {
			emit(true)
		}
	}
	if !placed {
		extra := curOff - len(cells)
		curAt, curX = len(out), min(x+max(extra, 0), w-1)
	}
	emit(false)
	return out, curAt, curX
}

// Resizes the main screen to h rows at the same width, rows moving between the screen and the scrollback
func (e *Emulator) resizeHeight(h int) {
	s := e.main
	oh := len(s.lines)
	switch {
	case h < oh:
		drop := oh - h
		for drop > 0 && len(s.lines)-1 > s.cur.y && s.lines[len(s.lines)-1].used() == 0 && s.lines[len(s.lines)-1].Marks == 0 {
			s.lines = s.lines[:len(s.lines)-1]
			drop--
		}
		for i := 0; i < drop; i++ {
			e.pushBack(s.lines[0])
			s.lines = s.lines[1:]
			s.cur.y--
		}
	case h > oh:
		pull := min(h-oh, e.back.n)
		for i := 0; i < pull; i++ {
			l := *e.back.at(e.back.n - 1)
			e.back.n--
			s.lines = append([]Line{l}, s.lines...)
			s.cur.y++
		}
		for len(s.lines) < h {
			s.lines = append(s.lines, newLine(e.w))
		}
	}
	s.fit(e.w, h)
}
