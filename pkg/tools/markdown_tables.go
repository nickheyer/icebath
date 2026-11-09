package tools

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
)

// Narrowest column, the divider's minimum dash run
const minTableColWidth = 3

// wcswidth of the text clamped to zero or more
func dispWidth(s string) int {
	w := pyunicode.WcsWidth(s)
	if w < 0 {
		return 0
	}
	return w
}

// Cells of a pipe delimited table row with the outer pipes removed and each cell stripped
func SplitTableRow(row string) []string {
	s := pystr.Strip(row)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	cells := strings.Split(s, "|")
	for i, c := range cells {
		cells[i] = pystr.Strip(c)
	}
	return cells
}

// True for an optional colon, three or more dashes and an optional colon
func dividerCell(cell string) bool {
	c := pystr.Strip(cell)
	c = strings.TrimPrefix(c, ":")
	c = strings.TrimSuffix(c, ":")
	return len(c) >= 3 && strings.Trim(c, "-") == ""
}

// True when the row is a markdown table separator line
func IsTableDivider(row string) bool {
	cells := SplitTableRow(row)
	if len(cells) <= 1 {
		return false
	}
	for _, c := range cells {
		if !dividerCell(c) {
			return false
		}
	}
	return true
}

// True when the stripped row starts with a pipe or holds at least two pipes
func LooksLikeTableRow(row string) bool {
	s := pystr.Strip(row)
	return s != "" && (strings.HasPrefix(s, "|") || strings.Count(s, "|") >= 2)
}

// Header, divider and body rows padded to uniform display widths, vertical when wider than the terminal
func renderTableBlock(rows [][]string, availableWidth *int) []string {
	ncols := 0
	for _, r := range rows {
		if len(r) > ncols {
			ncols = len(r)
		}
	}
	padded := make([][]string, len(rows))
	for i, r := range rows {
		p := make([]string, ncols)
		copy(p, r)
		padded[i] = p
	}
	widths := make([]int, ncols)
	for c := 0; c < ncols; c++ {
		w := minTableColWidth
		for _, r := range padded {
			if dw := dispWidth(r[c]); dw > w {
				w = dw
			}
		}
		widths[c] = w
	}
	if availableWidth != nil {
		total := 0
		for _, w := range widths {
			total += w
		}
		if total+3*ncols+1 > max(*availableWidth, 20) {
			return renderVertical(padded, ncols, *availableWidth)
		}
	}
	row := func(cells []string) string {
		parts := make([]string, len(cells))
		for k, c := range cells {
			parts[k] = c + strings.Repeat(" ", max(0, widths[k]-dispWidth(c)))
		}
		return "| " + strings.Join(parts, " | ") + " |"
	}
	divs := make([]string, ncols)
	for k, w := range widths {
		divs[k] = strings.Repeat("-", w+2)
	}
	out := []string{row(padded[0]), "|" + strings.Join(divs, "|") + "|"}
	for _, r := range padded[1:] {
		out = append(out, row(r))
	}
	return out
}

// Pieces of one over wide word, each at most w display cells
func hardBreak(word string, w int) []string {
	var out []string
	buf, bw := "", 0
	for _, ch := range word {
		s := string(ch)
		cw := dispWidth(s)
		if cw == 0 {
			cw = 1
		}
		if bw+cw > w && buf != "" {
			out = append(out, buf)
			buf, bw = s, cw
		} else {
			buf += s
			bw += cw
		}
	}
	if buf != "" {
		out = append(out, buf)
	}
	return out
}

// Words of text wrapped at word boundaries to width display cells, over wide words hard broken
func wrapToWidth(text string, width int) []string {
	if width <= 0 || text == "" {
		return []string{text}
	}
	words := pystr.SplitWhitespace(text, -1)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	current, currentW := "", 0
	start := func(word string, ww int) {
		if ww <= width {
			current, currentW = word, ww
			return
		}
		pieces := hardBreak(word, width)
		current = ""
		if len(pieces) > 0 {
			lines = append(lines, pieces[:len(pieces)-1]...)
			current = pieces[len(pieces)-1]
		}
		currentW = dispWidth(current)
	}
	for _, word := range words {
		ww := dispWidth(word)
		switch {
		case current == "":
			start(word, ww)
		case currentW+1+ww <= width:
			current += " " + word
			currentW += 1 + ww
		default:
			lines = append(lines, current)
			start(word, ww)
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// Too wide table as label and value blocks under a thin rule, continuation lines indented two spaces
func renderVertical(rows [][]string, ncols int, availableWidth int) []string {
	if len(rows) == 0 {
		return nil
	}
	labels := make([]string, ncols)
	for i := range labels {
		if i < len(rows[0]) && rows[0][i] != "" {
			labels[i] = rows[0][i]
		} else {
			labels[i] = fmt.Sprintf("Column %d", i+1)
		}
	}
	sepLen := 30
	if availableWidth != 0 {
		sepLen = max(20, min(40, availableWidth-2))
	}
	separator := strings.Repeat("─", sepLen)
	contBudget := max(10, availableWidth-2)
	var out []string
	for ri, row := range rows[1:] {
		if ri > 0 {
			out = append(out, separator)
		}
		for ci, label := range labels {
			value := ""
			if ci < len(row) {
				value = row[ci]
			}
			if value == "" {
				out = append(out, label+":")
				continue
			}
			wrapped := wrapToWidth(value, max(10, availableWidth-dispWidth(label)-2))
			out = append(out, label+": "+wrapped[0])
			if len(wrapped) > 1 {
				for _, cl := range wrapToWidth(strings.Join(wrapped[1:], " "), contBudget) {
					if pystr.Strip(cl) != "" {
						out = append(out, "  "+cl)
					}
				}
			}
		}
	}
	return out
}

// Every pipe table under a divider rewritten with width aware padding, other lines verbatim
func RealignMarkdownTables(text string, availableWidth *int) string {
	if !strings.Contains(text, "|") {
		return text
	}
	lines := strings.Split(text, "\n")
	n := len(lines)
	var out []string
	for i := 0; i < n; {
		line := lines[i]
		if strings.Contains(line, "|") && i+1 < n && IsTableDivider(lines[i+1]) {
			header := SplitTableRow(line)
			var body [][]string
			j := i + 2
			for j < n && strings.Contains(lines[j], "|") && pystr.Strip(lines[j]) != "" {
				if !IsTableDivider(lines[j]) {
					body = append(body, SplitTableRow(lines[j]))
				}
				j++
			}
			anyHeader := false
			for _, h := range header {
				if h != "" {
					anyHeader = true
					break
				}
			}
			if anyHeader || len(body) > 0 {
				out = append(out, renderTableBlock(append([][]string{header}, body...), availableWidth)...)
				i = j
				continue
			}
		}
		out = append(out, line)
		i++
	}
	return strings.Join(out, "\n")
}
