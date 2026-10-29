package term

import (
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Width method the grid measures graphemes with, matching the host renderer
var widthOf = uv.WidthMethod(ansi.GraphemeWidth)

// DEC special graphics, the box drawing set programs select with ESC ( 0
var graphics = map[rune]rune{
	'`': '◆', 'a': '▒', 'b': '␉', 'c': '␌', 'd': '␍', 'e': '␊', 'f': '°', 'g': '±', 'h': '␤', 'i': '␋',
	'j': '┘', 'k': '┐', 'l': '┌', 'm': '└', 'n': '┼', 'o': '⎺', 'p': '⎻', 'q': '─', 'r': '⎼', 's': '⎽',
	't': '├', 'u': '┤', 'v': '┴', 'w': '┬', 'x': '│', 'y': '≤', 'z': '≥', '{': 'π', '|': '≠', '}': '£', '~': '·',
}

// Rune as the character set in force maps it
func (e *Emulator) translate(r rune) rune {
	c := &e.scr.cur
	set := c.g[c.gl]
	if c.ss != 0 {
		set = c.g[c.ss]
		c.ss = 0
	} else if r >= 0xa0 && r <= 0xff && c.gr >= 0 {
		set = c.g[c.gr]
	}
	switch set {
	case charsetGraphics:
		if m, ok := graphics[r]; ok {
			return m
		}
	case charsetUK:
		if r == '#' {
			return '£'
		}
	}
	return r
}

// Reports whether the rune extends the grapheme before it rather than starting a cell of its own
func joins(r rune) bool {
	switch {
	case r == 0x200d, r == 0xfe0f, r == 0xfe0e:
		return true
	case r >= 0x1f3fb && r <= 0x1f3ff:
		return true
	case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Mc, r):
		return true
	case r >= 0xe0020 && r <= 0xe007f:
		return true
	}
	return false
}

func regional(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }

// Puts a printable rune at the cursor, wrapping, inserting and joining graphemes as the modes ask
func (e *Emulator) print(r rune) {
	r = e.translate(r)
	s := e.scr
	c := &s.cur
	if joins(r) || e.joiner || (regional(r) && e.lastRegional()) {
		if e.join(r) {
			e.joiner = r == 0x200d
			return
		}
	}
	e.joiner = r == 0x200d
	if e.joiner {
		return
	}
	w := widthOf.StringWidth(string(r))
	if w <= 0 {
		return
	}
	w = min(w, 2)
	if c.wrap && e.modes.autowrap {
		e.wrapLine()
	}
	if w == 2 && c.x+1 > s.right {
		if e.modes.autowrap {
			s.line(c.y).set(c.x, blank(c.pen))
			e.wrapLine()
		} else {
			c.x = max(s.right-1, s.left)
		}
	}
	if e.modes.insert {
		e.insertCells(w)
	}
	cell := uv.Cell{Content: string(r), Width: w, Style: c.pen, Link: c.link}
	l := s.line(c.y)
	l.set(c.x, cell)
	if e.protect || l.guard != nil {
		for i := 0; i < w; i++ {
			l.setGuard(c.x+i, e.protect)
		}
	}
	e.advance(w)
}

// Moves the cursor right by w columns after a print, pending a wrap at the right margin
func (e *Emulator) advance(w int) {
	s := e.scr
	c := &s.cur
	if c.x+w > s.right {
		c.x = s.right
		c.wrap = e.modes.autowrap
		return
	}
	c.x += w
	c.wrap = false
}

// Ends the row with a soft wrap and moves to the start of the next row
func (e *Emulator) wrapLine() {
	s := e.scr
	s.line(s.cur.y).Wrapped = true
	s.cur.x = s.left
	s.cur.wrap = false
	e.index()
}

// Reports whether the cell before the cursor holds one regional indicator awaiting its pair
func (e *Emulator) lastRegional() bool {
	cell := e.lastCell()
	if cell == nil {
		return false
	}
	rs := []rune(cell.Content)
	return len(rs) == 1 && regional(rs[0])
}

// Cell the last print landed in, nil when the cursor sits at the start of a fresh row
func (e *Emulator) lastCell() *uv.Cell {
	s := e.scr
	c := s.cur
	x := c.x
	if !c.wrap {
		x--
	}
	for x >= 0 {
		cell := &s.line(c.y).Cells[x]
		if cell.Width > 0 {
			return cell
		}
		x--
	}
	if c.y > 0 && s.line(c.y-1).Wrapped {
		prev := s.line(c.y - 1)
		for x := len(prev.Cells) - 1; x >= 0; x-- {
			if prev.Cells[x].Width > 0 {
				return &prev.Cells[x]
			}
		}
	}
	return nil
}

// Appends the rune to the grapheme of the last cell, widening it when the cluster grew, false when there is none
func (e *Emulator) join(r rune) bool {
	cell := e.lastCell()
	if cell == nil {
		return false
	}
	content := cell.Content + string(r)
	w := min(max(widthOf.StringWidth(content), 1), 2)
	cell.Content = content
	if w == cell.Width {
		return true
	}
	s := e.scr
	c := &s.cur
	l := s.line(c.y)
	x := -1
	for i := range l.Cells {
		if &l.Cells[i] == cell {
			x = i
		}
	}
	if x < 0 {
		return true
	}
	if w == 2 && x+1 <= s.right {
		if l.Cells[x+1].Width == 2 {
			l.set(x+1, blank(c.pen))
		}
		l.Cells[x+1] = uv.Cell{}
		cell.Width = 2
		if !c.wrap {
			e.advance(1)
		}
	}
	return true
}

// Shifts the cells from the cursor right by n inside the margins, dropping those pushed past the right margin
func (e *Emulator) insertCells(n int) {
	s := e.scr
	c := s.cur
	if c.x < s.left || c.x > s.right {
		return
	}
	l := s.line(c.y)
	n = min(n, s.right-c.x+1)
	if s.right+1 <= len(l.Cells)-1 && l.Cells[s.right+1].Width == 0 && l.Cells[s.right].Width == 2 {
		l.set(s.right, blank(c.pen))
	}
	l.move(c.x+n, c.x, s.right+1-n-c.x)
	l.fill(c.x, c.x+n-1, blank(c.pen))
	e.splitWide(l, c.x+n)
	e.splitWide(l, s.right+1)
}

// Blanks a wide character whose halves fell on either side of column x
func (e *Emulator) splitWide(l *Line, x int) {
	if x <= 0 || x >= len(l.Cells) {
		return
	}
	if l.Cells[x].Width == 0 && l.Cells[x-1].Width != 2 {
		l.Cells[x] = uv.EmptyCell
	}
	if l.Cells[x-1].Width == 2 && l.Cells[x].Width != 0 {
		l.Cells[x-1] = uv.EmptyCell
	}
}

// Removes n cells at the cursor inside the margins, blanks filling in from the right margin
func (e *Emulator) deleteCells(n int) {
	s := e.scr
	c := s.cur
	if c.x < s.left || c.x > s.right {
		return
	}
	l := s.line(c.y)
	n = min(n, s.right-c.x+1)
	if l.Cells[c.x].Width == 0 {
		l.set(c.x, blank(c.pen))
	}
	l.move(c.x, c.x+n, s.right+1-n-c.x)
	l.fill(s.right-n+1, s.right, blank(c.pen))
	e.splitWide(l, c.x)
	e.splitWide(l, s.right+1)
}

// Blanks n cells from the cursor
func (e *Emulator) eraseCells(n int) {
	s := e.scr
	c := s.cur
	s.line(c.y).fill(c.x, min(c.x+n-1, e.w-1), blank(c.pen))
}

// Moves the cursor down a row, scrolling the region when it sits on the bottom margin
func (e *Emulator) index() {
	s := e.scr
	c := &s.cur
	switch {
	case c.y == s.bot:
		e.scrollRegionUp(1)
	case c.y < e.h-1:
		c.y++
	}
	c.wrap = false
}

// Moves the cursor up a row, scrolling the region down when it sits on the top margin
func (e *Emulator) reverseIndex() {
	s := e.scr
	c := &s.cur
	switch {
	case c.y == s.top:
		e.scrollRegionDown(1)
	case c.y > 0:
		c.y--
	}
	c.wrap = false
}

// Scrolls the region up n rows, rows leaving a full width region at the top of the screen going to the scrollback
func (e *Emulator) scrollRegionUp(n int) {
	s := e.scr
	left, right := e.hMargins()
	if s.top == 0 && left == 0 && right == e.w-1 && s == e.main {
		for i := 0; i < min(n, s.bot-s.top+1); i++ {
			e.pushBack(s.lines[s.top+i].clone())
		}
	}
	s.scrollUp(s.top, s.bot, left, right, n, blank(s.cur.pen))
}

// Scrolls the region down n rows
func (e *Emulator) scrollRegionDown(n int) {
	s := e.scr
	left, right := e.hMargins()
	s.scrollDown(s.top, s.bot, left, right, n, blank(s.cur.pen))
}

// Left and right margins in force, the whole width unless margins mode is on
func (e *Emulator) hMargins() (int, int) {
	if e.modes.margins {
		return e.scr.left, e.scr.right
	}
	return 0, e.w - 1
}

// Moves the cursor to the start of the row, the left margin when it sits at or past it
func (e *Emulator) carriageReturn() {
	s := e.scr
	c := &s.cur
	if c.x >= s.left || !e.modes.margins {
		c.x = s.left
	} else {
		c.x = 0
	}
	c.wrap = false
}

// Feeds a line, returning to the start of the row too in newline mode
func (e *Emulator) lineFeed() {
	e.index()
	if e.modes.newline {
		e.carriageReturn()
	}
}

// Moves the cursor left a column, back onto the row above in reverse wrap mode
func (e *Emulator) backspace() {
	s := e.scr
	c := &s.cur
	if c.wrap {
		c.wrap = false
		return
	}
	left := s.left
	if c.x < left {
		left = 0
	}
	if c.x > left {
		c.x--
		return
	}
	if e.modes.reverseWrap && e.modes.autowrap && c.y > s.top {
		c.y--
		c.x = s.right
	}
}

// Moves the cursor to the next tab stop
func (e *Emulator) tab() {
	s := e.scr
	c := &s.cur
	c.x = s.nextTab(c.x)
	c.wrap = false
}

// Places the cursor at the column and row, both zero based and clamped, honoring origin mode
func (e *Emulator) moveTo(x, y int) {
	s := e.scr
	c := &s.cur
	top, bot := 0, e.h-1
	left, right := 0, e.w-1
	if c.origin {
		top, bot = s.top, s.bot
		if e.modes.margins {
			left, right = s.left, s.right
		}
		x += left
		y += top
	}
	c.x = min(max(x, left), right)
	c.y = min(max(y, top), bot)
	c.wrap = false
}

// Moves the cursor by dx columns and dy rows, stopping at the margins the cursor is inside
func (e *Emulator) moveBy(dx, dy int) {
	s := e.scr
	c := &s.cur
	top, bot := 0, e.h-1
	if c.y >= s.top && c.y <= s.bot {
		top, bot = s.top, s.bot
	}
	left, right := 0, e.w-1
	if e.modes.margins && c.x >= s.left && c.x <= s.right {
		left, right = s.left, s.right
	}
	c.x = min(max(c.x+dx, left), right)
	c.y = min(max(c.y+dy, top), bot)
	c.wrap = false
}

// Blanks the screen below the cursor for 0, above it for 1, all of it for 2 and the scrollback for 3
func (e *Emulator) eraseDisplay(mode int, selective bool) {
	s := e.scr
	c := s.cur
	fill := blank(c.pen)
	w := e.w
	switch mode {
	case 0:
		e.eraseRow(c.y, c.x, w-1, fill, selective)
		for y := c.y + 1; y < e.h; y++ {
			e.eraseRow(y, 0, w-1, fill, selective)
		}
	case 1:
		for y := 0; y < c.y; y++ {
			e.eraseRow(y, 0, w-1, fill, selective)
		}
		e.eraseRow(c.y, 0, c.x, fill, selective)
	case 2:
		for y := 0; y < e.h; y++ {
			e.eraseRow(y, 0, w-1, fill, selective)
		}
	case 3:
		e.first += uint64(e.back.n)
		e.back.clear()
	}
	s.cur.wrap = false
}

// Blanks part of the row as EL does, 0 right of the cursor, 1 left of it and 2 the whole row
func (e *Emulator) eraseLine(mode int, selective bool) {
	s := e.scr
	c := s.cur
	fill := blank(c.pen)
	switch mode {
	case 0:
		e.eraseRow(c.y, c.x, e.w-1, fill, selective)
	case 1:
		e.eraseRow(c.y, 0, c.x, fill, selective)
	case 2:
		e.eraseRow(c.y, 0, e.w-1, fill, selective)
	}
	s.cur.wrap = false
}

// Blanks columns of a row, the whole span or only the cells not protected when selective
func (e *Emulator) eraseRow(y, from, to int, fill uv.Cell, selective bool) {
	l := e.scr.line(y)
	if !selective {
		l.fill(from, to, fill)
		if from == 0 && to >= e.w-1 {
			l.Wrapped = false
		}
		return
	}
	for x := max(from, 0); x <= to && x < len(l.Cells); x++ {
		if !l.guarded(x) {
			l.set(x, fill)
		}
	}
}

// Inserts n blank rows at the cursor inside the scroll region
func (e *Emulator) insertLines(n int) {
	s := e.scr
	c := s.cur
	if c.y < s.top || c.y > s.bot {
		return
	}
	left, right := e.hMargins()
	if e.modes.margins && (c.x < s.left || c.x > s.right) {
		return
	}
	s.scrollDown(c.y, s.bot, left, right, n, blank(c.pen))
	s.cur.x = left
	s.cur.wrap = false
}

// Deletes n rows at the cursor inside the scroll region
func (e *Emulator) deleteLines(n int) {
	s := e.scr
	c := s.cur
	if c.y < s.top || c.y > s.bot {
		return
	}
	left, right := e.hMargins()
	if e.modes.margins && (c.x < s.left || c.x > s.right) {
		return
	}
	s.scrollUp(c.y, s.bot, left, right, n, blank(c.pen))
	s.cur.x = left
	s.cur.wrap = false
}

// Inserts n blank columns at the cursor across the scroll region
func (e *Emulator) insertColumns(n int) {
	s := e.scr
	c := s.cur
	left, right := e.hMargins()
	if c.x < left || c.x > right || c.y < s.top || c.y > s.bot {
		return
	}
	n = min(n, right-c.x+1)
	for y := s.top; y <= s.bot; y++ {
		l := s.line(y)
		l.move(c.x+n, c.x, right+1-n-c.x)
		l.fill(c.x, c.x+n-1, blank(c.pen))
		e.splitWide(l, c.x+n)
		e.splitWide(l, right+1)
	}
}

// Deletes n columns at the cursor across the scroll region
func (e *Emulator) deleteColumns(n int) {
	s := e.scr
	c := s.cur
	left, right := e.hMargins()
	if c.x < left || c.x > right || c.y < s.top || c.y > s.bot {
		return
	}
	n = min(n, right-c.x+1)
	for y := s.top; y <= s.bot; y++ {
		l := s.line(y)
		l.move(c.x, c.x+n, right+1-n-c.x)
		l.fill(right-n+1, right, blank(c.pen))
		e.splitWide(l, c.x)
		e.splitWide(l, right+1)
	}
}

// Fills the screen with E, the DECALN alignment test
func (e *Emulator) alignmentTest() {
	s := e.scr
	cell := uv.Cell{Content: "E", Width: 1}
	for y := range s.lines {
		s.lines[y] = Line{Cells: s.lines[y].Cells}
		s.lines[y].fill(0, e.w-1, cell)
	}
	s.top, s.bot = 0, e.h-1
	s.left, s.right = 0, e.w-1
	s.cur.x, s.cur.y, s.cur.wrap = 0, 0, false
}

// Saves the cursor with its pen, character sets and origin mode
func (e *Emulator) saveCursor() {
	e.scr.saved = e.scr.cur
	e.scr.kept = true
}

// Restores the saved cursor, the home position when none was saved
func (e *Emulator) restoreCursor() {
	s := e.scr
	if !s.kept {
		s.cur = cursor{g: s.cur.g, gl: s.cur.gl, gr: s.cur.gr}
		e.moveTo(0, 0)
		return
	}
	saved := s.saved
	s.cur = saved
	s.cur.x = min(max(saved.x, 0), e.w-1)
	s.cur.y = min(max(saved.y, 0), e.h-1)
	s.cur.wrap = false
	s.cur.ss = 0
}

// Shows the alternate screen or the main one, clearing and saving the cursor as the mode asks
func (e *Emulator) switchScreen(alt, clear, save bool) {
	if alt == e.modes.alt {
		if alt && clear {
			e.alt.clear(blank(e.scr.cur.pen))
		}
		return
	}
	if alt {
		if save {
			e.saveCursor()
		}
		e.alt.cur = e.main.cur
		e.alt.top, e.alt.bot = 0, e.h-1
		e.alt.left, e.alt.right = 0, e.w-1
		e.scr = e.alt
		if clear {
			e.alt.clear(blank(e.scr.cur.pen))
		}
	} else {
		if clear {
			e.alt.clear(blank(e.scr.cur.pen))
		}
		e.main.cur = e.alt.cur
		e.scr = e.main
		if save {
			e.restoreCursor()
		}
	}
	e.modes.alt = alt
	e.joiner = false
}
