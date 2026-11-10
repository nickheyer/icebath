package app

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// panel accumulates the styled fragments of one bordered overlay panel
type panel struct {
	lines  []styledFragment
	border string
	width  int
}

// A panel with its top rule, the title inlined into the rule when given
func newPanel(border string, boxWidth int, title, titleStyle string) *panel {
	p := &panel{border: border, width: boxWidth}
	if title != "" {
		p.lines = append(p.lines, styledFragment{border, "╭─ "}, styledFragment{titleStyle, title},
			styledFragment{border, " " + strings.Repeat("─", max(0, boxWidth-pystr.Len(title)-3)) + "╮\n"})
		return p
	}
	p.lines = append(p.lines, styledFragment{border, "╭" + strings.Repeat("─", boxWidth) + "╮\n"})
	return p
}

// Appends one content row
func (p *panel) row(style, text string) { appendPanelLine(&p.lines, p.border, style, text, p.width) }

// Appends one blank row
func (p *panel) blank() { appendBlankPanelLine(&p.lines, p.border, p.width) }

// Appends the bottom rule and returns every fragment
func (p *panel) close() []styledFragment {
	p.lines = append(p.lines, styledFragment{p.border, "╰" + strings.Repeat("─", p.width) + "╯\n"})
	return p.lines
}
