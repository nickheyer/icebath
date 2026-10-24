package render

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Column of a table with how its cells align
type Column struct {
	Title string
	// Share of spare width the column takes, zero fitting it to its content
	Flex int
	// Right aligns the cells, for numbers
	Right bool
	// Largest width the column grows to, unlimited when zero
	Max int
}

// Cell of the selected row being typed into in place, its text shown selected until the first keystroke
type Edit struct {
	Col      int
	Text     string
	Selected bool
}

// Columns a table keeps clear of the left edge and between cells
const (
	tableGutter = 2
	tableGap    = 3
)

// Rows the table spends above its first row, the header and the rule under it
const TableHead = 2

// Narrowest a flex column shrinks to before columns are dropped from the right
const minFlex = 8

// Left edge and width of every column, zero for columns dropped from the right to fit
func Columns(cols []Column, rows [][]string, width int) (xs, widths []int) {
	widths = make([]int, len(cols))
	for i, c := range cols {
		widths[i] = ansi.StringWidth(c.Title)
		for _, r := range rows {
			if i < len(r) {
				widths[i] = max(widths[i], ansi.StringWidth(r[i]))
			}
		}
		if c.Max > 0 {
			widths[i] = min(widths[i], c.Max)
		}
	}
	avail := width - tableGutter - 1
	kept := len(cols)
	for ; kept > 1; kept-- {
		if fit(cols[:kept], widths[:kept], avail) {
			break
		}
	}
	if kept == 1 && !fit(cols[:1], widths[:1], avail) {
		widths[0] = max(avail, 1)
	}
	for i := kept; i < len(cols); i++ {
		widths[i] = 0
	}
	xs = make([]int, len(cols))
	x := tableGutter
	for i := range cols {
		xs[i] = x
		if i < kept {
			x += widths[i] + tableGap
		}
	}
	return xs, widths
}

// Fits the columns into avail by growing or shrinking flex columns, reporting whether they fit
func fit(cols []Column, widths []int, avail int) bool {
	total := tableGap * (len(cols) - 1)
	flex, shrinkable := 0, 0
	for i, c := range cols {
		total += widths[i]
		if c.Flex > 0 {
			flex += c.Flex
			shrinkable += max(widths[i]-minFlex, 0)
		}
	}
	if total <= avail {
		if spare := avail - total; flex > 0 {
			for i, c := range cols {
				if c.Flex > 0 {
					widths[i] += spare * c.Flex / flex
				}
			}
		}
		return true
	}
	over := total - avail
	if shrinkable < over {
		return false
	}
	for i, c := range cols {
		if c.Flex == 0 || over <= 0 {
			continue
		}
		cut := min(over, max(widths[i]-minFlex, 0))
		widths[i] -= cut
		over -= cut
	}
	return true
}

// Renders rows under headers at width, cutting cells to their column and marking the selected row
func Table(cols []Column, rows [][]string, width int, selected int, t theme.Theme) string {
	return TableEdit(cols, rows, width, selected, nil, t)
}

// Renders the table with one cell of the selected row shown as it is being typed into
func TableEdit(cols []Column, rows [][]string, width int, selected int, edit *Edit, t theme.Theme) string {
	return TableMarked(cols, rows, width, selected, nil, edit, t)
}

// Renders the table with the cursor row lit and every other marked row tinted as part of the selection
func TableMarked(cols []Column, rows [][]string, width int, selected int, marked func(int) bool, edit *Edit, t theme.Theme) string {
	if len(cols) == 0 || width <= 0 {
		return ""
	}
	_, widths := Columns(cols, rows, width)
	head := lipgloss.NewStyle().Foreground(t.TextDim).Bold(true)
	body := lipgloss.NewStyle().Foreground(t.Text)
	sel := lipgloss.NewStyle().Foreground(t.Text).Background(t.Steel).Bold(true)
	mark := lipgloss.NewStyle().Foreground(t.Ice).Background(t.Steel).Bold(true)
	field := lipgloss.NewStyle().Foreground(t.Ice).Background(t.Bg).Bold(true)
	chosen := lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true)
	tinted := lipgloss.NewStyle().Foreground(t.Text).Background(t.Ghost)
	tick := lipgloss.NewStyle().Foreground(t.Ice).Background(t.Ghost).Bold(true)
	gap := strings.Repeat(" ", tableGap)
	last := 0
	for i := range cols {
		if widths[i] > 0 {
			last = i
		}
	}
	line := func(cells []string, st lipgloss.Style, gutter string, ed *Edit) string {
		var sb strings.Builder
		sb.WriteString(gutter)
		for i, c := range cols {
			if widths[i] == 0 {
				continue
			}
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			if ed != nil && ed.Col == i {
				text := ed.Text
				if n := ansi.StringWidth(text); n > widths[i]-1 {
					text = ansi.TruncateLeft(text, n-widths[i]+1, "")
				}
				pad := widths[i] - ansi.StringWidth(text) - 1
				if ed.Selected && text != "" {
					sb.WriteString(chosen.Render(text))
				} else {
					sb.WriteString(field.Render(text))
				}
				sb.WriteString(field.Render(t.Glyphs.Caret + strings.Repeat(" ", max(pad, 0))))
			} else {
				cell = Truncate(cell, widths[i], t)
				pad := widths[i] - ansi.StringWidth(cell)
				if c.Right {
					cell = strings.Repeat(" ", pad) + cell
				} else {
					cell += strings.Repeat(" ", pad)
				}
				sb.WriteString(Paint(st, cell))
			}
			if i < last {
				sb.WriteString(st.Render(gap))
			}
		}
		out := sb.String()
		if pad := width - ansi.StringWidth(out); pad > 0 {
			out += st.Render(strings.Repeat(" ", pad))
		}
		return ansi.Truncate(out, width, "")
	}
	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = strings.ToUpper(c.Title)
	}
	var sb strings.Builder
	sb.WriteString(line(titles, head, head.Render(strings.Repeat(" ", tableGutter)), nil))
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat(t.Glyphs.Rule, width)))
	for i, r := range rows {
		sb.WriteString("\n")
		if i != selected && marked != nil && marked(i) {
			sb.WriteString(line(r, tinted, tick.Render(t.Glyphs.Marked)+tinted.Render(strings.Repeat(" ", tableGutter-1)), nil))
			continue
		}
		if i != selected {
			sb.WriteString(line(r, body, body.Render(strings.Repeat(" ", tableGutter)), nil))
			continue
		}
		sb.WriteString(line(r, sel, mark.Render(t.Glyphs.Cursor)+sel.Render(strings.Repeat(" ", tableGutter-1)), edit))
	}
	return sb.String()
}
