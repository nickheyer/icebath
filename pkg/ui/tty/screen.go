package tty

// Terminal as tty last left it, every op it writes applied here the way the terminal applies it
type screen struct {
	w, h  int
	cells []Cell
	known []bool
	// Cursor column and row, each trusted only while its flag holds
	cx, cy     int
	xok, yok   bool
	pen        Style
	link       uint32
	penok      bool
	dec, decok bool
	// Pixel image drawn since the last erase of the whole screen, which any print over it would cut
	pixels bool
	out    []byte
	links  *linkTable
}

// Screen of unknown content, cursor and pen
func newScreen(w, h int, links *linkTable) *screen {
	s := &screen{links: links}
	s.resize(w, h)
	return s
}

// Takes a new size, forgetting everything shown
func (s *screen) resize(w, h int) {
	s.w, s.h = w, h
	if cap(s.cells) < w*h {
		s.cells = make([]Cell, w*h)
		s.known = make([]bool, w*h)
	}
	s.cells, s.known = s.cells[:w*h], s.known[:w*h]
	s.forget()
}

// Marks every cell, the cursor, the pen and the charset unknown
func (s *screen) forget() {
	for i := range s.known {
		s.known[i] = false
	}
	s.disturb()
}

// Marks the cursor, pen and charset unknown after bytes tty did not write
func (s *screen) disturb() {
	s.xok, s.yok, s.penok, s.decok = false, false, false, false
}

// Row y of the model
func (s *screen) row(y int) []Cell { return s.cells[y*s.w : (y+1)*s.w] }

// Known flags of row y
func (s *screen) rowKnown(y int) []bool { return s.known[y*s.w : (y+1)*s.w] }

// Queues raw bytes that change no cell, cursor or pen
func (s *screen) raw(b string) { s.out = append(s.out, b...) }

// Cursor position, each coordinate trusted only while its flag holds
type cursor struct {
	x, y     int
	xok, yok bool
}

// Cursor as the model holds it
func (s *screen) cursor() cursor { return cursor{s.cx, s.cy, s.xok, s.yok} }

// Appends the cheapest motion from the cursor to x, y on a screen h rows high with no margins set
func motion(b []byte, c cursor, x, y, h int) []byte {
	if c.xok && c.yok && c.x == x && c.y == y {
		return b
	}
	var scratch [48]byte
	best := cup(scratch[:0], x, y)
	if !c.yok {
		return append(b, best...)
	}
	try := func(seq []byte) {
		if len(seq) < len(best) {
			best = append(best[:0:0], seq...)
		}
	}
	if c.y == y {
		try(horiz(nil, c.x, c.xok, x))
		return append(b, best...)
	}
	var vert [][]byte
	if dy := y - c.y; dy > 0 {
		if dy <= 4 && y < h {
			vert = append(vert, repeatByte('\n', dy))
		}
		vert = append(vert, csiN('B', dy))
	} else {
		vert = append(vert, csiN('A', -dy))
	}
	vert = append(vert, append(num([]byte("\x1b["), y+1), 'd'))
	for _, v := range vert {
		if c.xok && c.x == x {
			try(v)
		}
		if x == 0 {
			try(append([]byte{'\r'}, v...))
		}
		try(append(append([]byte(nil), v...), horiz(nil, c.x, c.xok, x)...))
	}
	return append(b, best...)
}

// Bytes the cheapest motion from the cursor to x, y costs
func motionCost(c cursor, x, y, h int) int {
	var buf [48]byte
	return len(motion(buf[:0], c, x, y, h))
}

// Cheapest motion within the row from column fx, known or not, to column x
func horiz(b []byte, fx int, fxok bool, x int) []byte {
	best := append(num(append(b[:0:0], "\x1b["...), x+1), 'G')
	if x == 0 {
		best = []byte{'\r'}
	}
	if !fxok {
		return append(b, best...)
	}
	try := func(c []byte) {
		if len(c) < len(best) {
			best = c
		}
	}
	switch dx := x - fx; {
	case dx == 0:
		return b
	case dx > 0:
		try(csiN('C', dx))
	default:
		if -dx <= 3 {
			try(repeatByte('\b', -dx))
		}
		try(csiN('D', -dx))
	}
	return append(b, best...)
}

// CUP to column x of row y, shortened at the home position and the first column
func cup(b []byte, x, y int) []byte {
	b = append(b, "\x1b["...)
	switch {
	case x == 0 && y == 0:
	case x == 0:
		b = num(b, y+1)
	default:
		b = append(num(b, y+1), ';')
		b = num(b, x+1)
	}
	return append(b, 'H')
}

// CSI with a count, the count left out when it is one
func csiN(final byte, n int) []byte {
	b := []byte("\x1b[")
	if n != 1 {
		b = num(b, n)
	}
	return append(b, final)
}

// The same byte n times
func repeatByte(c byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return b
}

// Moves the cursor to x, y by the cheapest sequence
func (s *screen) moveTo(x, y int) {
	if !(s.xok && s.yok && s.cx == x && s.cy == y) {
		s.out = motion(s.out, s.cursor(), x, y, s.h)
	}
	s.cx, s.cy, s.xok, s.yok = x, y, true, true
}

// Sets the pen and hyperlink by the shortest sequences
func (s *screen) setPen(st Style, link uint32) {
	if !s.penok || s.pen != st {
		s.out = appendSGR(s.out, s.pen, s.penok, st)
	}
	if !s.penok || s.link != link {
		s.out = s.links.appendOSC8(s.out, link)
	}
	s.pen, s.link, s.penok = st, link, true
}

// Selects the charset a print needs, DEC special graphics or ASCII
func (s *screen) charset(dec bool) {
	if s.decok && s.dec == dec {
		return
	}
	if dec {
		s.raw("\x1b(0")
	} else {
		s.raw("\x1b(B")
	}
	s.dec, s.decok = dec, true
}

// Marks unknown the cluster halves a write over columns a to b would split
func (s *screen) unsplit(y, a, b int) {
	row, known := s.row(y), s.rowKnown(y)
	for x := a; x > 0 && x < s.w && row[x].covered(); x-- {
		known[x-1] = false
	}
	for x := b; x < s.w && row[x].covered(); x++ {
		known[x] = false
	}
}

// Prints a cell in an encoding at the cursor, the cursor moving by the width the terminal gives it
func (s *screen) print(c Cell, e enc) {
	x, y := s.cx, s.cy
	dec := e == encDEC
	s.charset(dec)
	s.ride(c.Rider, x, y, dec)
	switch e {
	case encDEC:
		s.out = append(s.out, decGraphics[c.Text])
	case encSized:
		s.out = append(s.out, "\x1b]66;w="...)
		s.out = num(s.out, int(c.Width))
		s.out = append(s.out, ';')
		s.out = append(s.out, c.Text...)
		s.out = append(s.out, '\a')
	default:
		s.out = append(s.out, c.Text...)
	}
	w := int(c.Width)
	back := x + w
	if back >= s.w {
		back = -1
	}
	s.ride(c.After, back, y, dec)
	s.unsplit(y, x, x+w)
	row, known := s.row(y), s.rowKnown(y)
	for i := 0; i < w && x+i < s.w; i++ {
		if i == 0 {
			row[x] = c
		} else {
			row[x+i] = coveredCell
		}
		known[x+i] = true
	}
	if back < 0 {
		s.xok = false
		return
	}
	s.cx = back
}

// Prints a cell of unmeasured width, leaving the column and every cell it may reach unknown
func (s *screen) printBlind(c Cell) {
	x, y := s.cx, s.cy
	s.charset(false)
	s.ride(c.Rider, x, y, false)
	s.out = append(s.out, c.Text...)
	s.ride(c.After, -1, y, false)
	end := min(x+widthBound(c.Text), s.w)
	s.unsplit(y, x, end)
	known := s.rowKnown(y)
	for i := x; i < end; i++ {
		known[i] = false
	}
	s.xok = false
}

// Writes a sequence riding with a cell, putting back the pen, charset and the cursor at x, y when it moved them
func (s *screen) ride(seq string, x, y int, dec bool) {
	if seq == "" {
		return
	}
	pen, link, penok := s.pen, s.link, s.penok
	s.raw(seq)
	e := effectOf([]byte(seq))
	s.pixels = s.pixels || e.pixels
	if !e.cursor {
		return
	}
	s.disturb()
	if x >= 0 {
		s.moveTo(x, y)
	}
	if penok {
		s.setPen(pen, link)
	}
	s.charset(dec)
}

// Repeats the last printed cell n more times with REP
func (s *screen) repeat(n int) {
	c := s.row(s.cy)[s.cx-1]
	s.out = append(s.out, csiN('b', n)...)
	x, y := s.cx, s.cy
	s.unsplit(y, x, x+n)
	row, known := s.row(y), s.rowKnown(y)
	for i := 0; i < n && x+i < s.w; i++ {
		row[x+i], known[x+i] = c, true
	}
	if x+n >= s.w {
		s.xok = false
		return
	}
	s.cx = x + n
}

// Erases n cells from the cursor with ECH, the pen already set to the blank style
func (s *screen) eraseChars(n int) {
	s.out = append(s.out, csiN('X', n)...)
	s.blankRange(s.cy, s.cx, min(s.cx+n, s.w))
}

// Erases from the cursor to the end of the row with EL
func (s *screen) eraseLine() {
	s.raw("\x1b[K")
	s.blankRange(s.cy, s.cx, s.w)
}

// Erases the whole screen with ED 2, pixel images with it
func (s *screen) eraseScreen() {
	s.raw("\x1b[2J")
	for y := 0; y < s.h; y++ {
		s.blankRange(y, 0, s.w)
	}
	s.pixels = false
}

// Marks columns a to b of row y blank in the pen's background, known only when the pen holds nothing else
func (s *screen) blankRange(y, a, b int) {
	s.unsplit(y, a, b)
	blank := Cell{Text: " ", Width: 1, Style: Style{Bg: s.pen.Bg}}
	plain := s.penok && s.link == 0 && s.pen == blank.Style
	row, known := s.row(y), s.rowKnown(y)
	for x := a; x < b; x++ {
		row[x], known[x] = blank, plain
	}
}

// Shifts rows a to b up by n with the rows vacated at the bottom left unknown
func (s *screen) shiftUp(a, b, n int) {
	for y := a; y <= b-n; y++ {
		copy(s.row(y), s.row(y+n))
		copy(s.rowKnown(y), s.rowKnown(y+n))
	}
	for y := max(b-n+1, a); y <= b; y++ {
		s.unknownRow(y)
	}
}

// Shifts rows a to b down by n with the rows vacated at the top left unknown
func (s *screen) shiftDown(a, b, n int) {
	for y := b; y >= a+n; y-- {
		copy(s.row(y), s.row(y-n))
		copy(s.rowKnown(y), s.rowKnown(y-n))
	}
	for y := a; y < min(a+n, b+1); y++ {
		s.unknownRow(y)
	}
}

// Marks every cell of row y unknown
func (s *screen) unknownRow(y int) {
	known := s.rowKnown(y)
	for x := range known {
		known[x] = false
	}
}

// Deletes n rows at row y with DL, rows below moving up
func (s *screen) deleteLines(y, n int) {
	s.moveTo(0, y)
	s.out = append(s.out, csiN('M', n)...)
	s.shiftUp(y, s.h-1, n)
	s.xok = false
}

// Inserts n rows at row y with IL, rows below moving down and the last ones dropped
func (s *screen) insertLines(y, n int) {
	s.moveTo(0, y)
	s.out = append(s.out, csiN('L', n)...)
	s.shiftDown(y, s.h-1, n)
	s.xok = false
}

// Scrolls rows a to b by n inside DECSTBM margins with LF or RI, positive n moving content up
func (s *screen) scrollRegion(a, b, n int) {
	full := a == 0 && b == s.h-1
	if !full {
		s.out = append(s.out, "\x1b["...)
		s.out = append(num(s.out, a+1), ';')
		s.out = append(num(s.out, b+1), 'r')
		s.xok, s.yok = false, false
	}
	if n > 0 {
		s.moveTo(0, b)
		s.out = append(s.out, repeatByte('\n', n)...)
		s.shiftUp(a, b, n)
	} else {
		s.moveTo(0, a)
		for i := 0; i < -n; i++ {
			s.raw("\x1bM")
		}
		s.shiftDown(a, b, -n)
	}
	if !full {
		s.raw("\x1b[r")
		s.xok, s.yok = false, false
	}
}
