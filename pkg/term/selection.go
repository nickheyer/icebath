package term

import (
	"strings"
	"unicode"
)

// Cell position by absolute row and column
type Pos struct {
	Row uint64
	Col int
}

// Reports whether p comes before q in reading order
func (p Pos) before(q Pos) bool {
	return p.Row < q.Row || (p.Row == q.Row && p.Col < q.Col)
}

// How a selection grows from its anchor
type SelMode int

const (
	SelChars SelMode = iota
	SelWords
	SelLines
	SelBlock
)

// Stretch of cells between an anchor and the head the pointer moves, both inclusive
type Selection struct {
	Anchor Pos
	Head   Pos
	Mode   SelMode
}

// Ends in reading order
func (s *Selection) ordered() (Pos, Pos) {
	if s.Head.before(s.Anchor) {
		return s.Head, s.Anchor
	}
	return s.Anchor, s.Head
}

// Columns the selection covers on the row, false when it misses the row
func (s *Selection) span(row uint64, w int, l *Line) (int, int, bool) {
	a, b := s.ordered()
	if row < a.Row || row > b.Row {
		return 0, 0, false
	}
	switch s.Mode {
	case SelBlock:
		return min(a.Col, b.Col), max(a.Col, b.Col), true
	case SelLines:
		return 0, w - 1, true
	}
	from, to := 0, w-1
	if row == a.Row {
		from = a.Col
	}
	if row == b.Row {
		to = b.Col
	}
	if l != nil && row != b.Row && l.Wrapped {
		to = w - 1
	}
	return from, to, true
}

// Characters that hold a word together beyond letters and digits
const wordChars = "_-./~?&=%+#@:"

func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(wordChars, r)
}

// Text the selection covers, soft wrapped rows joined and trailing blanks of each row dropped
func (e *Emulator) Text(s Selection) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, b := s.ordered()
	var sb strings.Builder
	for row := a.Row; row <= b.Row; row++ {
		l, ok := e.row(row)
		if !ok {
			break
		}
		from, to, ok := s.span(row, len(l.Cells), l)
		if !ok {
			continue
		}
		var line strings.Builder
		for x := max(from, 0); x <= to && x < len(l.Cells); x++ {
			c := &l.Cells[x]
			if c.Width == 0 {
				continue
			}
			line.WriteString(c.Content)
		}
		text := line.String()
		joined := s.Mode != SelBlock && l.Wrapped && row != b.Row
		if !joined || to < len(l.Cells)-1 {
			text = strings.TrimRight(text, " ")
		}
		sb.WriteString(text)
		if row != b.Row && !joined {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// Start and end of the word under the position, crossing soft wraps, the position alone on a blank
func (e *Emulator) WordAt(p Pos) (Pos, Pos) {
	e.mu.Lock()
	defer e.mu.Unlock()
	kind := func(q Pos) int {
		l, ok := e.row(q.Row)
		if !ok || q.Col < 0 || q.Col >= len(l.Cells) {
			return -1
		}
		c := &l.Cells[q.Col]
		if c.Width == 0 && q.Col > 0 {
			c = &l.Cells[q.Col-1]
		}
		rs := []rune(c.Content)
		switch {
		case len(rs) == 0 || rs[0] == ' ':
			return 0
		case wordRune(rs[0]):
			return 1
		}
		return 2
	}
	want := kind(p)
	if want <= 0 {
		return p, p
	}
	from, to := p, p
	for {
		q, ok := e.step(from, -1)
		if !ok || kind(q) != want {
			break
		}
		from = q
	}
	for {
		q, ok := e.step(to, 1)
		if !ok || kind(q) != want {
			break
		}
		to = q
	}
	if l, ok := e.row(to.Row); ok && to.Col+1 < len(l.Cells) && l.Cells[to.Col+1].Width == 0 {
		to.Col++
	}
	return from, to
}

// Position one cell before or after p along the logical row, false at its ends
func (e *Emulator) step(p Pos, d int) (Pos, bool) {
	l, ok := e.row(p.Row)
	if !ok {
		return p, false
	}
	w := len(l.Cells)
	if d < 0 {
		if p.Col > 0 {
			return Pos{p.Row, p.Col - 1}, true
		}
		if p.Row == e.first {
			return p, false
		}
		prev, ok := e.row(p.Row - 1)
		if !ok || !prev.Wrapped {
			return p, false
		}
		return Pos{p.Row - 1, len(prev.Cells) - 1}, true
	}
	if p.Col < w-1 {
		return Pos{p.Row, p.Col + 1}, true
	}
	if !l.Wrapped {
		return p, false
	}
	if _, ok := e.row(p.Row + 1); !ok {
		return p, false
	}
	return Pos{p.Row + 1, 0}, true
}

// First and last position of the logical row holding p, soft wraps included
func (e *Emulator) LineAt(p Pos) (Pos, Pos) {
	e.mu.Lock()
	defer e.mu.Unlock()
	from, to := Pos{p.Row, 0}, Pos{p.Row, e.w - 1}
	for from.Row > e.first {
		prev, ok := e.row(from.Row - 1)
		if !ok || !prev.Wrapped {
			break
		}
		from.Row--
	}
	for {
		l, ok := e.row(to.Row)
		if !ok || !l.Wrapped {
			break
		}
		if _, ok := e.row(to.Row + 1); !ok {
			break
		}
		to.Row++
	}
	if l, ok := e.row(to.Row); ok {
		to.Col = len(l.Cells) - 1
	}
	return from, to
}

// Absolute row of the nearest prompt mark above or below the row, false when there is none
func (e *Emulator) Prompt(from uint64, d int) (uint64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	last := e.base() + uint64(len(e.scr.lines)) - 1
	row := from
	for {
		if d < 0 {
			if row == e.first {
				return 0, false
			}
			row--
		} else {
			if row >= last {
				return 0, false
			}
			row++
		}
		if l, ok := e.row(row); ok && l.Marks&MarkPrompt != 0 {
			return row, true
		}
	}
}
