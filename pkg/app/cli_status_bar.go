package app

import (
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
)

// Terminal cells the text occupies, wide glyphs counting two and control characters none
func statusBarDisplayWidth(text string) int {
	width := 0
	for _, r := range pystr.Runes(text) {
		width += max(0, pyunicode.WcWidth(r))
	}
	return width
}

// Full viewport width for printed scrollback box rules, floored at thirty two columns
func scrollbackBoxWidth() int { return max(32, terminalColumns()) }
