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
}

// Appends one row of markup cells
func (t *Table) AddRow(cells ...string) { t.Rows = append(t.Rows, cells) }

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
