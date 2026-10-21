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

// Renders rows under headers at width, cutting cells to their column and highlighting the selected row
func Table(cols []Column, rows [][]string, width int, selected int, t theme.Theme) string {
	if len(cols) == 0 || width <= 0 {
		return ""
	}
	widths := make([]int, len(cols))
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
	gap := 1
	total := gap * (len(cols) - 1)
	flex := 0
	for i, c := range cols {
		total += widths[i]
		flex += c.Flex
	}
	if total > width {
		over := total - width
		for i := range cols {
			if over <= 0 {
				break
			}
			if cols[i].Flex > 0 || i == len(cols)-1 {
				cut := min(over, widths[i]-3)
				if cut > 0 {
					widths[i] -= cut
					over -= cut
				}
			}
		}
		for i := len(cols) - 1; i >= 0 && over > 0; i-- {
			cut := min(over, widths[i]-1)
			if cut > 0 {
				widths[i] -= cut
				over -= cut
			}
		}
	} else if flex > 0 {
		spare := width - total
		for i, c := range cols {
			if c.Flex > 0 {
				widths[i] += spare * c.Flex / flex
			}
		}
	}
	head := lipgloss.NewStyle().Foreground(t.TextDim).Bold(true)
	body := lipgloss.NewStyle().Foreground(t.Text)
	sel := lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true)
	line := func(cells []string, st lipgloss.Style) string {
		parts := make([]string, len(cols))
		for i, c := range cols {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			cell = Truncate(cell, widths[i])
			pad := widths[i] - ansi.StringWidth(cell)
			if c.Right {
				cell = strings.Repeat(" ", pad) + cell
			} else {
				cell += strings.Repeat(" ", pad)
			}
			parts[i] = cell
		}
		return st.Render(strings.Join(parts, strings.Repeat(" ", gap)))
	}
	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.Title
	}
	var sb strings.Builder
	sb.WriteString(line(titles, head))
	for i, r := range rows {
		sb.WriteString("\n")
		st := body
		if i == selected {
			st = sel
		}
		sb.WriteString(line(r, st))
	}
	return sb.String()
}
