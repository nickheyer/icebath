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
	words := strings.FieldsFunc(plain, pyIsSpace)
	if len(words) == 0 {
		m.min = m.max
	}
	for _, w := range words {
		m.min = max(m.min, CellLen(w))
	}
	return m
}

// True for the characters Python str.split treats as whitespace, the information separators included
func pyIsSpace(r rune) bool { return (r >= 0x1c && r <= 0x1f) || unicode.IsSpace(r) }

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
	pad := t.padLeft(i) + t.padRight(i)
	if col.Width > 0 {
		w := min(col.Width+t.paddingWidth(i), maxWidth)
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
		m.min = max(m.min, cm.min+pad)
		m.max = max(m.max, cm.max+pad)
	}
	m.min, m.max = min(m.min, maxWidth), min(m.max, maxWidth)
	if col.MaxWidth > 0 {
		limit := col.MaxWidth + t.paddingWidth(i)
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
func cellLines(markup, style string, width int, col Column, color bool, system string) []string {
	if style != "" {
		markup = "[" + style + "]" + markup + "[/" + style + "]"
	}
	overflow := col.Overflow
	if overflow == "" {
		overflow = "ellipsis"
	}
	rendered := RenderSystem(markup, color, system)
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
func (t *Table) RenderWidth(color bool, width int) string { return t.renderWidth(color, "", width) }

// Lines of the table fitted to width cells with styles rendered for the named color system
func (t *Table) renderWidth(color bool, system string, width int) string {
	extra := t.edgeCells()
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
		if t.NoBox {
			edge = ""
		}
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
			split[i] = cellLines(text, style, w-t.padLeft(i)-t.padRight(i), col, color, system)
			height = max(height, len(split[i]))
		}
		out := make([]string, height)
		for r := range height {
			var b strings.Builder
			b.WriteString(edge)
			for i, w := range widths {
				left, right := t.padLeft(i), t.padRight(i)
				cell := strings.Repeat(" ", w-left-right)
				if r < len(split[i]) {
					cell = split[i][r]
				}
				b.WriteString(strings.Repeat(" ", left) + cell + strings.Repeat(" ", right) + edge)
			}
			out[r] = b.String()
		}
		return out
	}
	var lines []string
	if t.Title != "" {
		for _, l := range WrapStyled(RenderSystem("[italic]"+t.Title+"[/italic]", color, system), tableWidth, true) {
			l = strings.TrimRightFunc(l, unicode.IsSpace)
			n := CellLen(ansiRE.ReplaceAllString(l, ""))
			left := max(0, (tableWidth-n)/2)
			lines = append(lines, strings.Repeat(" ", left)+l+strings.Repeat(" ", max(0, tableWidth-n-left)))
		}
	}
	if !t.NoBox {
		lines = append(lines, rule("┏", "━", "┳", "┓"))
	}
	if !t.HideHeader {
		headers := make([]string, len(t.Columns))
		for i, c := range t.Columns {
			headers[i] = Escape(c.Header)
		}
		lines = append(lines, row("┃", headers, true)...)
		if !t.NoBox {
			lines = append(lines, rule("┡", "━", "╇", "┩"))
		}
	}
	for _, r := range t.Rows {
		lines = append(lines, row("│", r, false)...)
	}
	if !t.NoBox {
		lines = append(lines, rule("└", "─", "┴", "┘"))
	}
	return strings.Join(lines, "\n")
}

// Rich panel with the rounded box expanded to the console width, title and subtitle centered in the border
type Panel struct {
	Body, Title, Subtitle, BorderStyle string
	// Panel sized to its content as Panel(expand=False) instead of the full width
	Fit bool
	// Renderable drawn in place of Body when set, as a Markdown body
	Content Renderable
	// Horizontal padding inside the border on each side, the rich default of one when zero
	PadX int
	// Box drawn around the body, horizontals for rules above and below only, rounded when empty
	Box string
	// Side the title sits on in the top border, left or right, centered when empty
	TitleAlign string
	// Style laid under the border, the padding and the body, as Panel(style=...)
	Style string
	// Vertical then horizontal padding replacing PadX when set, as Panel(padding=(v, h))
	Padding *[2]int
	// Outer width capped at the console width, the console width when zero
	Width int
}

// Cells of padding between the border and the body on each side
func (p Panel) pad() int {
	if p.Padding != nil {
		return max(0, p.Padding[1])
	}
	if p.PadX > 0 {
		return p.PadX
	}
	return 1
}

// Width a fitted panel takes, the body measure plus padding widened for the title and capped at width
func (p Panel) fitWidth(color bool, width int) int {
	child := width - 2
	if p.Content != nil {
		child = min(p.Content.MeasureMax(width-2)+2*p.pad(), width-2)
	} else {
		longest := 0
		for _, l := range strings.Split(Render(p.Body, color), "\n") {
			longest = max(longest, CellLen(ansiRE.ReplaceAllString(l, "")))
		}
		child = min(min(longest, width-2)+2*p.pad(), width-2)
	}
	if p.Title != "" {
		child = min(width-2, max(child, CellLen(Plain(strings.ReplaceAll(p.Title, "\n", " ")))+4))
	}
	return child + 2
}

// Lines of the panel at width cells
func (p Panel) Render(color bool, width int) string { return p.render(color, "", width) }

// Panel lines as a console prints a Panel, the body never repr highlighted
func (p Panel) render(color bool, system string, width int) string {
	if p.Width > 0 {
		width = min(width, p.Width)
	}
	base, _ := ParseStyle(p.Style)
	styled := base != (Style{})
	borderStyle := base
	if bs, err := ParseStyle(p.BorderStyle); err == nil {
		borderStyle = base.Add(bs)
	}
	under := func(s string) string {
		if s == "" || !styled || !color {
			return s
		}
		return base.Render(s, resolveSystem(system))
	}
	border := func(s string) string {
		if s == "" || !color || (p.BorderStyle == "" && !styled) {
			return s
		}
		if styled {
			return borderStyle.Render(s, resolveSystem(system))
		}
		return RenderSystem("["+p.BorderStyle+"]"+Escape(s)+"[/]", color, system)
	}
	left, right, bottomLeft, bottomRight, side := "╭", "╮", "╰", "╯", "│"
	if p.Box == "horizontals" {
		left, right, bottomLeft, bottomRight, side = " ", " ", " ", " ", " "
	}
	edge := func(left, right, title string) string {
		if title == "" || width <= 4 {
			return border(left + strings.Repeat("─", max(0, width-2)) + right)
		}
		text := " " + strings.ReplaceAll(title, "\n", " ") + " "
		if p.BorderStyle != "" {
			text = "[" + p.BorderStyle + "]" + text + "[/]"
		}
		rendered := truncateStyled(RenderSystem(text, color, system), width-4, "fold")
		if styled && color {
			rendered = decodeANSILine(rendered).over(base).render(color, resolveSystem(system))
		}
		n := CellLen(oscRE.ReplaceAllString(ansiRE.ReplaceAllString(rendered, ""), ""))
		excess := width - 4 - n
		l := excess / 2
		switch p.TitleAlign {
		case "left":
			l = 0
		case "right":
			l = excess
		}
		return border(left+"─") + border(strings.Repeat("─", l)) + rendered + border(strings.Repeat("─", excess-l)) + border("─"+right)
	}
	if p.Fit {
		width = p.fitWidth(color, width)
	}
	gap := strings.Repeat(" ", p.pad())
	inner := max(1, width-2-2*p.pad())
	padY := 0
	if p.Padding != nil {
		padY = max(0, p.Padding[0])
	}
	blank := border(side) + under(strings.Repeat(" ", max(0, width-2))) + border(side)
	lines := []string{edge(left, right, p.Title)}
	for range padY {
		lines = append(lines, blank)
	}
	body := WrapStyled(renderHighlighted(p.Body, color, false, system), inner, true)
	if p.Content != nil {
		contentSystem := ""
		if color {
			contentSystem = resolveSystem(system)
		}
		body = p.Content.RenderLines(color, contentSystem, inner)
		if sr, ok := p.Content.(spanRenderable); ok && styled {
			body = body[:0]
			for _, l := range sr.spanLines(inner, false) {
				body = append(body, l.over(base).render(color, contentSystem))
			}
		}
	}
	for _, l := range body {
		if p.Content == nil {
			l = truncateStyled(l, inner, "ellipsis")
		}
		if styled && color {
			if _, spans := p.Content.(spanRenderable); !spans {
				l = decodeANSILine(l).over(base).render(color, resolveSystem(system))
			}
		}
		pad := strings.Repeat(" ", max(0, inner-CellLen(oscRE.ReplaceAllString(ansiRE.ReplaceAllString(l, ""), ""))))
		lines = append(lines, border(side)+under(gap)+l+under(pad)+under(gap)+border(side))
	}
	for range padY {
		lines = append(lines, blank)
	}
	lines = append(lines, edge(bottomLeft, bottomRight, p.Subtitle))
	return strings.Join(lines, "\n")
}
