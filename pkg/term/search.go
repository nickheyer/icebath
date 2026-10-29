package term

import (
	"strings"
	"unicode"
)

// Matches a search returns at most
const maxMatches = 10000

// Columns a match covers on one row
type Match struct {
	Row      uint64
	From, To int
}

// Every place the text occurs, scrollback first, letters matched without regard to case when the query has no capitals
func (e *Emulator) Find(query string) []Match {
	e.mu.Lock()
	defer e.mu.Unlock()
	if query == "" {
		return nil
	}
	fold := strings.IndexFunc(query, unicode.IsUpper) < 0
	if fold {
		query = strings.ToLower(query)
	}
	var out []Match
	last := e.base() + uint64(len(e.scr.lines))
	for row := e.first; row < last && len(out) < maxMatches; {
		text, cols, next := e.logical(row, fold)
		for at := 0; at < len(text); {
			i := strings.Index(text[at:], query)
			if i < 0 {
				break
			}
			start, end := at+i, at+i+len(query)-1
			out = append(out, segments(cols[start:end+1])...)
			at = start + 1
			if len(out) >= maxMatches {
				break
			}
		}
		row = next
	}
	return out
}

// Text of the logical row starting at row with the position of every byte, and the row after it
func (e *Emulator) logical(row uint64, fold bool) (string, []Pos, uint64) {
	var sb strings.Builder
	var cols []Pos
	for {
		l, ok := e.row(row)
		if !ok {
			return sb.String(), cols, row + 1
		}
		for x, c := range l.Cells {
			if c.Width == 0 {
				continue
			}
			content := c.Content
			if fold {
				content = strings.ToLower(content)
			}
			for range len(content) {
				cols = append(cols, Pos{row, x})
			}
			sb.WriteString(content)
		}
		row++
		if !l.Wrapped {
			return sb.String(), cols, row
		}
	}
}

// Match spans over consecutive positions, one per row they cross
func segments(cols []Pos) []Match {
	var out []Match
	for _, p := range cols {
		if n := len(out); n > 0 && out[n-1].Row == p.Row {
			out[n-1].To = p.Col
			continue
		}
		out = append(out, Match{Row: p.Row, From: p.Col, To: p.Col})
	}
	return out
}
