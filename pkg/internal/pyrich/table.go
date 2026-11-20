package pyrich

import (
	"strings"
	"unicode/utf8"
)

// Column header and the style every cell of the column renders with
type Column struct {
	Header string
	Style  string
	// Fixed and maximum content widths in cells, zero when unset
	Width, MaxWidth int
	// Cell alignment left or right and the overflow of a word wider than the column, fold or ellipsis
	Justify, Overflow string
	NoWrap            bool
}

// Rich style table drawn with the heavy head box, a centered italic title and bold headers
type Table struct {
	Title   string
	Columns []Column
	Rows    [][]string
	// Header row left out as show_header=False
	HideHeader bool
	// Borders and separators left out as box=None, cells set apart by their padding alone
	NoBox bool
	// Horizontal padding on each side of a cell, the rich default of one when zero
	PadX int
	// Outer padding dropped and padding between columns collapsed with no box or header, as Table.grid
	Grid bool
}

// Grid of columns set apart by padX cells, as Table.grid(padding=(0, padX))
func NewGrid(padX int, columns ...Column) *Table {
	return &Table{Columns: columns, HideHeader: true, NoBox: true, PadX: padX, Grid: true}
}

// Cells of padding left of a column, none for the first grid column or a collapsed grid gap
func (t *Table) padLeft(i int) int {
	if t.Grid {
		return 0
	}
	return t.pad()
}

// Cells of padding right of a column, none for the last grid column
func (t *Table) padRight(i int) int {
	if t.Grid && i == len(t.Columns)-1 {
		return 0
	}
	return t.pad()
}

// Padding rich adds to a fixed, minimum or maximum column width, collapsed for grid columns after the first
func (t *Table) paddingWidth(i int) int {
	if t.Grid && i > 0 {
		return t.pad()
	}
	return 2 * t.pad()
}

// Appends one row of markup cells
func (t *Table) AddRow(cells ...string) { t.Rows = append(t.Rows, cells) }

// Horizontal cell padding per side
func (t *Table) pad() int {
	if t.PadX > 0 {
		return t.PadX
	}
	return 1
}

// Cells the vertical borders take, none without a box
func (t *Table) edgeCells() int {
	if t.NoBox {
		return 0
	}
	return len(t.Columns) + 1
}

// RenderLines draws the table at its natural width inside another renderable
func (t *Table) RenderLines(color bool, system string, width int) []string {
	return strings.Split(t.renderWidth(color, system, width), "\n")
}

// MeasureMax is the natural width of the table within width cells
func (t *Table) MeasureMax(width int) int {
	total := t.edgeCells()
	for _, w := range t.columnWidths(width - t.edgeCells()) {
		total += w
	}
	return total
}

// Lines of the table, styled with ANSI sequences when color is true
func (t *Table) Render(color bool) string {
	widths := make([]int, len(t.Columns))
	measure := func(i int, cell string) {
		for _, line := range strings.Split(Plain(cell), "\n") {
			widths[i] = max(widths[i], utf8.RuneCountInString(line))
		}
	}
	for i, c := range t.Columns {
		measure(i, Escape(c.Header))
	}
	for _, row := range t.Rows {
		for i := range t.Columns {
			if i < len(row) {
				measure(i, row[i])
			}
		}
	}
	rule := func(left, fill, mid, right string) string {
		parts := make([]string, len(widths))
		for i, w := range widths {
			parts[i] = strings.Repeat(fill, w+2)
		}
		return left + strings.Join(parts, mid) + right
	}
	styled := func(style, markup string) string {
		if style == "" {
			return markup
		}
		return "[" + style + "]" + markup + "[/" + style + "]"
	}
	line := func(edge, sep string, cells []string, header bool) []string {
		height := 1
		split := make([][]string, len(widths))
		for i := range widths {
			text := ""
			if i < len(cells) {
				text = cells[i]
			}
			split[i] = strings.Split(text, "\n")
			height = max(height, len(split[i]))
		}
		out := make([]string, height)
		for row := range height {
			var b strings.Builder
			b.WriteString(edge)
			for i, w := range widths {
				cell := ""
				if row < len(split[i]) {
					cell = split[i][row]
				}
				pad := strings.Repeat(" ", w-utf8.RuneCountInString(Plain(cell)))
				style := t.Columns[i].Style
				if header {
					style = "bold"
				}
				b.WriteString(" " + Render(styled(style, cell), color) + pad + " ")
				if i < len(widths)-1 {
					b.WriteString(sep)
				} else {
					b.WriteString(edge)
				}
			}
			out[row] = b.String()
		}
		return out
	}
	var lines []string
	if t.Title != "" {
		total := 1
		for _, w := range widths {
			total += w + 3
		}
		title := Plain(t.Title)
		left := max(0, (total-utf8.RuneCountInString(title))/2)
		lines = append(lines, strings.Repeat(" ", left)+Render(styled("italic", Escape(title)), color))
	}
	lines = append(lines, rule("┏", "━", "┳", "┓"))
	headers := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		headers[i] = Escape(c.Header)
	}
	lines = append(lines, line("┃", "┃", headers, true)...)
	lines = append(lines, rule("┡", "━", "╇", "┩"))
	for _, row := range t.Rows {
		lines = append(lines, line("│", "│", row, false)...)
	}
	lines = append(lines, rule("└", "─", "┴", "┘"))
	return strings.Join(lines, "\n")
}
