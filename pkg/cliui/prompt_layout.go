package cliui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Widths a list draws its rows at and the lines every row takes at them
type listLayout struct {
	body    int
	room    int
	name    int
	mark    int
	note    int
	stacked bool
	heights []int
}

// Columns before the text of a choice, the cursor mark and the check box of a multiple choice list
func (p *listPrompt) prefixWidth() int {
	if p.multi {
		return 2 + ansi.StringWidth(p.c.glyphs().checked) + 1
	}
	return 2
}

// Layout of the list for a body width, natural column widths when they fit, kept until the width changes
func (p *listPrompt) layout(body int) *listLayout {
	if p.laid != nil && p.laid.body == body {
		return p.laid
	}
	lay := &listLayout{body: body, room: max(body-p.prefixWidth(), 4), heights: make([]int, len(p.rows))}
	var table []int
	var widest [3]int
	for i, row := range p.rows {
		if row.index < 0 || row.primary == "" || row.cells[1].width+row.cells[2].width == 0 {
			continue
		}
		table = append(table, i)
		for k, cell := range row.cells {
			widest[k] = max(widest[k], cell.width)
		}
	}
	gaps := 0
	for _, w := range widest[1:] {
		if w > 0 {
			gaps += 2
		}
	}
	if widest[0]+widest[1]+widest[2]+gaps <= lay.room {
		lay.name, lay.mark, lay.note = widest[0], widest[1], widest[2]
	} else {
		p.fit(lay, table, widest, gaps)
	}
	for i, row := range p.rows {
		lay.heights[i] = len(p.rowLines(row, false, lay))
	}
	p.laid = lay
	return lay
}

// Lines each table row takes in one column at one width, and how many cells split a word there
type columnFit struct {
	heights []int
	split   int
}

// Picks the column widths, or the stacked form, with the fewest lines plus broken names, marks and split words
func (p *listPrompt) fit(lay *listLayout, table []int, widest [3]int, gaps int) {
	var memo [3]map[int]columnFit
	at := func(col, width int) columnFit {
		if memo[col] == nil {
			memo[col] = map[int]columnFit{}
		}
		if f, ok := memo[col][width]; ok {
			return f
		}
		f := columnFit{heights: make([]int, len(table))}
		for i, r := range table {
			cell := p.rows[r].cells[col]
			f.heights[i] = cell.height(width)
			if cell.word > width {
				f.split++
			}
		}
		memo[col][width] = f
		return f
	}
	best, fewest := -1, 0
	for name := widest[0]; name >= min(widest[0], 2); name-- {
		for mark := widest[1]; mark >= min(widest[1], 2); mark-- {
			note := min(widest[2], lay.room-gaps-name-mark)
			if note < min(widest[2], 2) {
				continue
			}
			names, marks, notes := at(0, name), at(1, mark), at(2, note)
			lines, broken := 0, names.split+marks.split+notes.split
			for i := range table {
				lines += max(names.heights[i], marks.heights[i], notes.heights[i])
				if names.heights[i] > 1 {
					broken++
				}
				if marks.heights[i] > 1 {
					broken++
				}
			}
			cost := lines + broken
			if best < 0 || cost < best || (cost == best && (broken < fewest || (broken == fewest && note > lay.note))) {
				best, fewest, lay.name, lay.mark, lay.note = cost, broken, name, mark, note
			}
		}
	}
	stacked := 0
	for _, r := range table {
		cells := p.rows[r].cells
		lines := cells[0].height(lay.room)
		if lines > 1 {
			stacked++
		}
		if cells[0].word > lay.room {
			stacked++
		}
		if cells[1].width > 0 && cells[0].width+2+cells[1].width > lay.room {
			below := cells[1].height(lay.room - 2)
			lines += below
			if below > 1 {
				stacked++
			}
		}
		for _, cell := range cells[1:] {
			if cell.word > lay.room-2 {
				stacked++
			}
		}
		stacked += lines + cells[2].height(lay.room-2)
	}
	if best < 0 || stacked < best {
		lay.stacked, lay.name, lay.mark, lay.note = true, 0, 0, 0
	}
}

// Most lines any run of count consecutive shown rows takes, one line at least
func (l *listLayout) tallest(shown []int, count int) int {
	best, sum := 1, 0
	for i, row := range shown {
		sum += l.heights[row]
		if i >= count {
			sum -= l.heights[shown[i-count]]
		}
		best = max(best, sum)
	}
	return best
}

// Styled runs of the explanation cell, led by the state mark when marks have no column of their own
func (p *listPrompt) noteSpans(row listRow) []span {
	t := activeTheme()
	note := span{row.secondary, textStyle{color: t.Muted}}
	if !row.markLeads {
		return []span{note}
	}
	mark := span{row.tag, textStyle{color: t.Frost}}
	if row.secondary == "" {
		return []span{mark}
	}
	return []span{mark, {"  ", textStyle{}}, note}
}

// Rows of a labeled choice, its name, state mark and explanation wrapped in columns or stacked under the name
func (p *listPrompt) cellRows(row listRow, atCursor bool, lay *listLayout) []string {
	t := activeTheme()
	nameStyle := textStyle{}
	if atCursor {
		nameStyle = textStyle{color: t.Frost, bold: true}
	}
	name := []span{{row.primary, nameStyle}}
	if row.cells[1].width+row.cells[2].width == 0 {
		return p.c.spanRows(row.cells[0], name, lay.room)
	}
	mark := []span{{row.cells[1].text, textStyle{color: t.Frost}}}
	note := p.noteSpans(row)
	if lay.stacked {
		out := p.c.spanRows(row.cells[0], name, lay.room)
		under := p.c.spanRows(row.cells[2], note, lay.room-2)
		if row.cells[1].width > 0 && row.cells[0].width+2+row.cells[1].width <= lay.room {
			out[0] += "  " + p.c.paint(mark[0].text, mark[0].style)
		} else {
			under = append(p.c.spanRows(row.cells[1], mark, lay.room-2), under...)
		}
		for _, line := range under {
			out = append(out, "  "+line)
		}
		return out
	}
	widths := [3]int{lay.name, lay.mark, lay.note}
	cols := [3][]string{p.c.spanRows(row.cells[0], name, lay.name), p.c.spanRows(row.cells[1], mark, lay.mark), p.c.spanRows(row.cells[2], note, lay.note)}
	var out []string
	for i := 0; i < max(len(cols[0]), len(cols[1]), len(cols[2])); i++ {
		var b strings.Builder
		pad := 0
		for k, rows := range cols {
			if widths[k] == 0 {
				continue
			}
			if i < len(rows) {
				b.WriteString(strings.Repeat(" ", pad))
				b.WriteString(rows[i])
				pad = -ansi.StringWidth(rows[i])
			}
			pad += widths[k] + 2
		}
		out = append(out, b.String())
	}
	return out
}

// Painted lines of one row at the widths of a layout, later lines hung under the row text
func (p *listPrompt) rowLines(row listRow, atCursor bool, lay *listLayout) []string {
	g, t := p.c.glyphs(), activeTheme()
	if row.index < 0 {
		heading := []span{{row.heading, textStyle{color: t.Muted, bold: true}}}
		return hung(p.c.spanRows(row.cells[0], heading, lay.body), "    ", "    ")
	}
	prefix := "  "
	if atCursor {
		prefix = p.c.paint(g.cursor, textStyle{color: t.Accent, bold: true}) + " "
	}
	if p.multi {
		box := p.c.paint(g.unchecked, textStyle{color: t.Faint})
		if p.checked[row.index] {
			box = p.c.paint(g.checked, textStyle{color: t.OK})
		}
		prefix += box + " "
	}
	hang := strings.Repeat(" ", 4+p.prefixWidth())
	if row.primary == "" {
		return hung(p.c.spanRows(row.cells[0], p.segments(row.item, atCursor), lay.room), "    "+prefix, hang)
	}
	return hung(p.cellRows(row, atCursor, lay), "    "+prefix, hang)
}

// Shown rows in view, scrolled the least that keeps the cursor row whole and the list end filling the view
func (p *listPrompt) window(lay *listLayout, shown []int, cursorPos, view int) (first, end int) {
	if len(shown) == 0 {
		p.scroll = 0
		return 0, 0
	}
	tall := func(i int) int { return lay.heights[shown[i]] }
	lowest := cursorPos
	for used := tall(cursorPos); lowest > 0 && used+tall(lowest-1) <= view; lowest-- {
		used += tall(lowest - 1)
	}
	tail := len(shown)
	for used := 0; tail > 0 && used+tall(tail-1) <= view; tail-- {
		used += tall(tail - 1)
	}
	first = min(max(min(p.scroll, cursorPos), lowest), tail)
	if first > 0 && first == cursorPos && p.rows[shown[first-1]].index < 0 && tall(first-1)+tall(first) <= view {
		first--
	}
	end = first
	for used := 0; end < len(shown) && used+tall(end) <= view; end++ {
		used += tall(end)
	}
	p.scroll = first
	return first, max(end, first+1)
}

// Lines given to the description and to the list out of the lines available, keep lines of choices coming first
func split(desc, avail, keep, want int) (descRows, view int) {
	if desc > 0 {
		descRows = min(desc, avail-keep)
		if descRows < 2 && descRows < desc {
			descRows = 0
		}
	}
	return descRows, min(want, max(avail-descRows, 1))
}
