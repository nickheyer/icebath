package render

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Lines beyond which a diff is summarized instead of computed
const MaxDiffLines = 6000

// Kind of one diff line
type Op int

const (
	Same Op = iota
	Added
	Removed
)

// One line of a diff with the line numbers it held before and after
type Line struct {
	Op   Op
	Text string
	Old  int
	New  int
}

// Lines changed between two texts, computed with a longest common subsequence over whole lines
func Diff(before, after string) []Line {
	a := splitLines(before)
	b := splitLines(after)
	if len(a)+len(b) > MaxDiffLines {
		out := make([]Line, 0, len(a)+len(b))
		for i, s := range a {
			out = append(out, Line{Op: Removed, Text: s, Old: i + 1})
		}
		for i, s := range b {
			out = append(out, Line{Op: Added, Text: s, New: i + 1})
		}
		return out
	}
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	ma, mb := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]
	n, m := len(ma), len(mb)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if ma[i] == mb[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	out := make([]Line, 0, len(a)+len(b))
	for i := 0; i < prefix; i++ {
		out = append(out, Line{Op: Same, Text: a[i], Old: i + 1, New: i + 1})
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && ma[i] == mb[j]:
			out = append(out, Line{Op: Same, Text: ma[i], Old: prefix + i + 1, New: prefix + j + 1})
			i++
			j++
		case i < n && (j >= m || lcs[i+1][j] >= lcs[i][j+1]):
			out = append(out, Line{Op: Removed, Text: ma[i], Old: prefix + i + 1})
			i++
		default:
			out = append(out, Line{Op: Added, Text: mb[j], New: prefix + j + 1})
			j++
		}
	}
	for k := 0; k < suffix; k++ {
		out = append(out, Line{Op: Same, Text: a[len(a)-suffix+k], Old: len(a) - suffix + k + 1, New: len(b) - suffix + k + 1})
	}
	return out
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// Counts of added and removed lines
func Stat(lines []Line) (added, removed int) {
	for _, l := range lines {
		switch l.Op {
		case Added:
			added++
		case Removed:
			removed++
		}
	}
	return added, removed
}

// Hunks of changed lines with the context around them, unchanged runs collapsed to a marker
func Hunks(lines []Line, context int) [][]Line {
	var hunks [][]Line
	var cur []Line
	since := -1
	flush := func() {
		if len(cur) > 0 {
			hunks = append(hunks, cur)
			cur = nil
		}
	}
	for i, l := range lines {
		if l.Op != Same {
			if since >= 0 && i-since > context {
				flush()
			}
			if cur == nil {
				for k := max(0, i-context); k < i; k++ {
					cur = append(cur, lines[k])
				}
			}
			cur = append(cur, l)
			since = i
			continue
		}
		if cur != nil && i-since <= context {
			cur = append(cur, l)
		}
	}
	flush()
	return hunks
}

// Renders a diff with line numbers, colors and hunk separators at width
func DiffText(before, after string, width int, t theme.Theme) string {
	lines := Diff(before, after)
	return FormatDiff(lines, width, t)
}

// Renders already computed diff lines
func FormatDiff(lines []Line, width int, t theme.Theme) string {
	added, removed := Stat(lines)
	if added == 0 && removed == 0 {
		return lipgloss.NewStyle().Foreground(t.TextDim).Render("no changes")
	}
	hunks := Hunks(lines, 3)
	num := lipgloss.NewStyle().Foreground(t.TextDim)
	add := lipgloss.NewStyle().Foreground(t.Alive)
	rem := lipgloss.NewStyle().Foreground(t.Alarm)
	same := lipgloss.NewStyle().Foreground(t.Text)
	sep := lipgloss.NewStyle().Foreground(t.Steel)
	digits := len(fmt.Sprint(max(len(lines), 1)))
	body := max(width-2*digits-4, 8)
	var sb strings.Builder
	for h, hunk := range hunks {
		if h > 0 {
			sb.WriteString(sep.Render(strings.Repeat("┄", min(width, 40))) + "\n")
		}
		for _, l := range hunk {
			oldN, newN, mark := " ", " ", " "
			st := same
			switch l.Op {
			case Added:
				newN, mark, st = fmt.Sprint(l.New), "+", add
			case Removed:
				oldN, mark, st = fmt.Sprint(l.Old), "-", rem
			default:
				oldN, newN = fmt.Sprint(l.Old), fmt.Sprint(l.New)
			}
			text := Truncate(expandTabs(l.Text), body)
			sb.WriteString(num.Render(fmt.Sprintf("%*s %*s ", digits, oldN, digits, newN)) + st.Render(mark+" "+text) + "\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func expandTabs(s string) string { return strings.ReplaceAll(s, "\t", "    ") }
