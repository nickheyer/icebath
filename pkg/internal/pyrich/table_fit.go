package pyrich

import (
	"strings"
	"unicode"
)

type measurement struct{ min, max int }

// Minimum and maximum cells of markup, the longest word and the longest line
func measureMarkup(markup string) measurement {
	plain := Plain(markup)
	var m measurement
	for _, line := range strings.Split(plain, "\n") {
		m.max = max(m.max, CellLen(line))
	}
	words := strings.FieldsFunc(plain, unicode.IsSpace)
	if len(words) == 0 {
		m.min = m.max
	}
	for _, w := range words {
		m.min = max(m.min, CellLen(w))
	}
	return m
}

// Rich ratio_reduce, taking total cells from values in proportion to ratios within maximums
func ratioReduce(total int, ratios, maximums, values []int) []int {
	ratios = append([]int(nil), ratios...)
	totalRatio := 0
	for i := range ratios {
		if maximums[i] == 0 {
			ratios[i] = 0
		}
		totalRatio += ratios[i]
	}
	if totalRatio == 0 {
		return append([]int(nil), values...)
	}
	remaining := total
	out := make([]int, len(values))
	for i, v := range values {
		if ratios[i] != 0 && totalRatio > 0 {
			distributed := min(maximums[i], pyRound(float64(ratios[i]*remaining)/float64(totalRatio)))
			out[i] = v - distributed
			remaining -= distributed
			totalRatio -= ratios[i]
		} else {
			out[i] = v
		}
	}
	return out
}

// Python round with ties to even
func pyRound(x float64) int {
	f := int(x)
	if x < 0 {
		f = int(x) - 1
		if float64(int(x)) == x {
			f = int(x)
		}
	}
	diff := x - float64(f)
	switch {
	case diff > 0.5:
		return f + 1
	case diff < 0.5:
		return f
	case f%2 == 0:
		return f
	}
	return f + 1
}

// Rich Table._collapse_widths, narrowing the widest wrappable columns first
func collapseWidths(widths []int, wrapable []bool, maxWidth int) []int {
	total := 0
	for _, w := range widths {
		total += w
	}
	excess := total - maxWidth
	anyWrap := false
	for _, w := range wrapable {
		anyWrap = anyWrap || w
	}
	if !anyWrap {
		return widths
	}
	for total > 0 && excess > 0 {
		maxCol := 0
		for i, w := range widths {
			if wrapable[i] {
				maxCol = max(maxCol, w)
			}
		}
		second := 0
		for i, w := range widths {
			if wrapable[i] && w != maxCol {
				second = max(second, w)
			}
		}
		diff := maxCol - second
		ratios := make([]int, len(widths))
		anyRatio := false
		for i, w := range widths {
			if w == maxCol && wrapable[i] {
				ratios[i] = 1
				anyRatio = true
			}
		}
		if !anyRatio || diff == 0 {
			break
		}
		maxReduce := make([]int, len(widths))
		for i := range maxReduce {
			maxReduce[i] = min(excess, diff)
		}
		widths = ratioReduce(excess, ratios, maxReduce, widths)
		total = 0
		for _, w := range widths {
			total += w
		}
		excess = total - maxWidth
	}
	return widths
}

// Column measurement with its one cell padding on each side, clamped like rich _measure_column
func (t *Table) measureColumn(i, maxWidth int) measurement {
	col := t.Columns[i]
	if col.Width > 0 {
		w := min(col.Width+2, maxWidth)
		return measurement{w, w}
	}
	cells := []string{Escape(col.Header)}
	for _, row := range t.Rows {
		if i < len(row) {
			cells = append(cells, row[i])
		} else {
			cells = append(cells, "")
		}
	}
	m := measurement{0, 0}
	for _, c := range cells {
		cm := measureMarkup(c)
		m.min = max(m.min, cm.min+2)
		m.max = max(m.max, cm.max+2)
	}
	m.min, m.max = min(m.min, maxWidth), min(m.max, maxWidth)
	if col.MaxWidth > 0 {
		limit := col.MaxWidth + 2
		m.min, m.max = min(m.min, limit), min(m.max, limit)
	}
	return m
}

// Column widths including padding that rich computes for a table no wider than maxWidth cells
func (t *Table) columnWidths(maxWidth int) []int {
	widths := make([]int, len(t.Columns))
	total := 0
	for i := range t.Columns {
		widths[i] = t.measureColumn(i, maxWidth).max
		if widths[i] == 0 {
			widths[i] = 1
		}
		total += widths[i]
	}
	if total > maxWidth {
		wrapable := make([]bool, len(t.Columns))
		for i, c := range t.Columns {
			wrapable[i] = c.Width == 0 && !c.NoWrap
		}
		widths = collapseWidths(widths, wrapable, maxWidth)
		total = 0
		for _, w := range widths {
			total += w
		}
		if total > maxWidth {
			ones := make([]int, len(widths))
			for i := range ones {
				ones[i] = 1
			}
			widths = ratioReduce(total-maxWidth, ones, widths, widths)
		}
		for i := range widths {
			widths[i] = t.measureColumn(i, widths[i]).max
		}
	}
	return widths
}

// Styled lines of one cell wrapped into width cells and justified
func cellLines(markup, style string, width int, col Column, color bool) []string {
	if style != "" {
		markup = "[" + style + "]" + markup + "[/" + style + "]"
	}
	overflow := col.Overflow
	if overflow == "" {
		overflow = "ellipsis"
	}
	rendered := Render(markup, color)
	var lines []string
	if col.NoWrap {
		lines = strings.Split(rendered, "\n")
	} else {
		lines = WrapStyled(rendered, width, overflow == "fold")
	}
	for i, l := range lines {
		if col.Justify == "right" {
			l = strings.TrimRightFunc(l, unicode.IsSpace)
		}
		l = truncateStyled(l, width, overflow)
		pad := strings.Repeat(" ", max(0, width-CellLen(ansiRE.ReplaceAllString(l, ""))))
		if col.Justify == "right" {
			lines[i] = pad + l
		} else {
			lines[i] = l + pad
		}
	}
	return lines
}

// Lines of the table fitted to width cells as rich renders it on a console that wide
func (t *Table) RenderWidth(color bool, width int) string {
	extra := len(t.Columns) + 1
	widths := t.columnWidths(width - extra)
	tableWidth := extra
	for _, w := range widths {
		tableWidth += w
	}
	rule := func(left, fill, mid, right string) string {
		parts := make([]string, len(widths))
		for i, w := range widths {
			parts[i] = strings.Repeat(fill, w)
		}
		return left + strings.Join(parts, mid) + right
	}
	row := func(edge string, cells []string, header bool) []string {
		split := make([][]string, len(widths))
		height := 1
		for i, w := range widths {
			text := ""
			if i < len(cells) {
				text = cells[i]
			}
			style := t.Columns[i].Style
			col := t.Columns[i]
			if header {
				style = "bold"
			}
			split[i] = cellLines(text, style, w-2, col, color)
			height = max(height, len(split[i]))
		}
		out := make([]string, height)
		for r := range height {
			var b strings.Builder
			b.WriteString(edge)
			for i, w := range widths {
				cell := strings.Repeat(" ", w-2)
				if r < len(split[i]) {
					cell = split[i][r]
				}
				b.WriteString(" " + cell + " " + edge)
			}
			out[r] = b.String()
		}
		return out
	}
	var lines []string
	if t.Title != "" {
		for _, l := range WrapStyled(Render("[italic]"+t.Title+"[/italic]", color), tableWidth, true) {
			l = strings.TrimRightFunc(l, unicode.IsSpace)
			n := CellLen(ansiRE.ReplaceAllString(l, ""))
			left := max(0, (tableWidth-n)/2)
			lines = append(lines, strings.Repeat(" ", left)+l+strings.Repeat(" ", max(0, tableWidth-n-left)))
		}
	}
	lines = append(lines, rule("┏", "━", "┳", "┓"))
	headers := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		headers[i] = Escape(c.Header)
	}
	lines = append(lines, row("┃", headers, true)...)
	lines = append(lines, rule("┡", "━", "╇", "┩"))
	for _, r := range t.Rows {
		lines = append(lines, row("│", r, false)...)
	}
	lines = append(lines, rule("└", "─", "┴", "┘"))
	return strings.Join(lines, "\n")
}

// Rich panel with the rounded box expanded to the console width, title and subtitle centered in the border
type Panel struct {
	Body, Title, Subtitle, BorderStyle string
}

// Lines of the panel at width cells
func (p Panel) Render(color bool, width int) string {
	border := func(s string) string {
		if p.BorderStyle == "" || !color {
			return s
		}
		return Render("["+p.BorderStyle+"]"+Escape(s)+"[/]", color)
	}
	edge := func(left, right, title string) string {
		if title == "" || width <= 4 {
			return border(left + strings.Repeat("─", max(0, width-2)) + right)
		}
		text := " " + strings.ReplaceAll(title, "\n", " ") + " "
		rendered := truncateStyled(Render(text, color), width-4, "ellipsis")
		n := CellLen(ansiRE.ReplaceAllString(rendered, ""))
		excess := width - 4 - n
		l := excess / 2
		return border(left+"─"+strings.Repeat("─", l)) + rendered + border(strings.Repeat("─", excess-l)+"─"+right)
	}
	inner := max(1, width-4)
	lines := []string{edge("╭", "╮", p.Title)}
	for _, l := range WrapStyled(Render(p.Body, color), inner, true) {
		l = truncateStyled(l, inner, "ellipsis")
		pad := strings.Repeat(" ", max(0, inner-CellLen(ansiRE.ReplaceAllString(l, ""))))
		lines = append(lines, border("│")+" "+l+pad+" "+border("│"))
	}
	lines = append(lines, edge("╰", "╯", p.Subtitle))
	return strings.Join(lines, "\n")
}
