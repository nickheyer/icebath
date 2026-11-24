package consoleutil

import (
	"math"
	"strings"
)

// One column of a table: its header, the style its cells draw in and how they fit the column
type Column struct {
	// Header text, printed as written in bold
	Header string
	// Style definition every cell of the column draws in
	Style string
	// Cells of content the column always takes, zero to fit the content
	Width int
	// Most cells of content the column takes, zero for no limit
	MaxWidth int
	// Side cells align to: left when empty, center or right
	Justify string
	// What a word wider than the column does: ellipsis when empty cuts it with an ellipsis, fold breaks it across lines, crop cuts it
	Overflow string
}

// Grid of markup cells under bold headers in a box with a heavy head, an italic title centered above it, fitted to the width it prints in
type Table struct {
	// Markup centered above the table
	Title   string
	Columns []Column
	// Header row left out
	HideHeader bool
	// Borders and rules left out, cells set apart by their padding alone
	NoBox bool
	// Cells of padding on each side of every cell, one when zero
	PadX int
	rows [][]string
}

// Appends one row of markup cells, missing cells left empty
func (t *Table) AddRow(cells ...string) { t.rows = append(t.rows, cells) }

func (t *Table) pad() int {
	if t.PadX > 0 {
		return t.PadX
	}
	return 1
}

// Cells the vertical borders take
func (t *Table) edgeCells() int {
	if t.NoBox {
		return 0
	}
	return len(t.Columns) + 1
}

// Styled header and body cells, header first when shown, each column's style under its body cells
func (t *Table) cells(o layout) [][]segments {
	var out [][]segments
	if !t.HideHeader {
		header := make([]segments, len(t.Columns))
		for i, c := range t.Columns {
			header[i] = segments(nil).with(stripEscapes(stripControl(c.Header)), Style{on: attrBold})
		}
		out = append(out, header)
	}
	styles := make([]Style, len(t.Columns))
	for i, c := range t.Columns {
		styles[i] = styleOf(c.Style)
	}
	for _, r := range t.rows {
		row := make([]segments, len(t.Columns))
		for i := range t.Columns {
			if i < len(r) {
				row[i] = markupText(r[i]).over(styles[i])
				if !o.links {
					row[i] = inlineLinks(row[i])
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// Narrowest and widest column i lays out within maxWidth cells, its padding included
func (t *Table) measureColumn(rows [][]segments, i, maxWidth int) measurement {
	col := t.Columns[i]
	pad := 2 * t.pad()
	if col.Width > 0 {
		w := min(col.Width+pad, maxWidth)
		return measurement{w, w}
	}
	m := measurement{pad, pad}
	for _, r := range rows {
		cm := measureText(r[i])
		m.min, m.max = max(m.min, cm.min+pad), max(m.max, cm.max+pad)
	}
	m.min, m.max = min(m.min, maxWidth), min(m.max, maxWidth)
	if col.MaxWidth > 0 {
		limit := col.MaxWidth + pad
		m.min, m.max = min(m.min, limit), min(m.max, limit)
	}
	return m
}

// Column widths with padding the table takes within maxWidth cells
func (t *Table) columnWidths(rows [][]segments, maxWidth int) []int {
	widths := make([]int, len(t.Columns))
	wrapable := make([]bool, len(t.Columns))
	for i, c := range t.Columns {
		widths[i] = max(1, t.measureColumn(rows, i, maxWidth).max)
		wrapable[i] = c.Width == 0
	}
	if sum(widths) <= maxWidth {
		return widths
	}
	widths = fitWidths(widths, wrapable, maxWidth)
	for i := range widths {
		widths[i] = t.measureColumn(rows, i, widths[i]).max
	}
	return widths
}

func (t *Table) measure(o layout) int {
	if len(t.Columns) == 0 {
		return 0
	}
	return t.edgeCells() + sum(t.columnWidths(t.cells(o), o.width-t.edgeCells()))
}

func (t *Table) render(o layout) []segments {
	if len(t.Columns) == 0 {
		return nil
	}
	rows := t.cells(o)
	widths := t.columnWidths(rows, o.width-t.edgeCells())
	tableWidth := t.edgeCells() + sum(widths)
	pad := t.pad()
	rule := func(left, fill, mid, right string) segments {
		parts := make([]string, len(widths))
		for i, w := range widths {
			parts[i] = strings.Repeat(fill, w)
		}
		return segments{{left + strings.Join(parts, mid) + right, Style{}}}
	}
	line := func(cells []segments, edge string) []segments {
		if t.NoBox {
			edge = ""
		}
		split := make([][]segments, len(widths))
		height := 1
		for i, w := range widths {
			col := t.Columns[i]
			overflow := col.Overflow
			if overflow == "" {
				overflow = "ellipsis"
			}
			justify := col.Justify
			if justify == "" {
				justify = "left"
			}
			if inner := w - 2*pad; inner > 0 {
				split[i] = wrap(cells[i], inner, wrapOptions{justify: justify, overflow: overflow})
			}
			height = max(height, len(split[i]))
		}
		out := make([]segments, height)
		for r := range height {
			l := segments(nil).with(edge, Style{})
			for i, w := range widths {
				left := min(pad, w)
				right := min(pad, w-left)
				inner := w - left - right
				l = l.with(spaces(left), Style{})
				if r < len(split[i]) {
					l = l.join(split[i][r].padTo(inner, Style{}))
				} else {
					l = l.with(spaces(inner), Style{})
				}
				l = l.with(spaces(right), Style{}).with(edge, Style{})
			}
			out[r] = l
		}
		return out
	}
	var lines []segments
	if t.Title != "" {
		title := markupText(t.Title).over(Style{on: attrItalic})
		if !o.links {
			title = inlineLinks(title)
		}
		for _, l := range wrap(title, tableWidth, wrapOptions{}) {
			l = l.rstrip()
			n := l.width()
			left := max(0, (tableWidth-n)/2)
			lines = append(lines, segments(nil).with(spaces(left), Style{}).join(l).with(spaces(tableWidth-n-left), Style{}))
		}
	}
	if !t.NoBox {
		lines = append(lines, rule("┏", "━", "┳", "┓"))
	}
	body := rows
	if !t.HideHeader {
		lines = append(lines, line(rows[0], "┃")...)
		if !t.NoBox {
			lines = append(lines, rule("┡", "━", "╇", "┩"))
		}
		body = rows[1:]
	}
	for _, r := range body {
		lines = append(lines, line(r, "│")...)
	}
	if !t.NoBox {
		lines = append(lines, rule("└", "─", "┴", "┘"))
	}
	return lines
}

// Lines of the table fitted to width cells, styled for the terminal when color is set
func (t *Table) RenderWidth(color bool, width int) string { return renderString(t, color, width) }

// Lines of the table at its natural width, styled for the terminal when color is set
func (t *Table) Render(color bool) string { return renderString(t, color, math.MaxInt32) }
