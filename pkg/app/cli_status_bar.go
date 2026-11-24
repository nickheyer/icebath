package app

import (
	"github.com/mattn/go-runewidth"
)

// Terminal cells the text occupies, wide glyphs counting two and control characters none
func statusBarDisplayWidth(text string) int {
	width := 0
	for _, r := range []rune(text) {
		width += max(0, (&runewidth.Condition{StrictEmojiNeutral: true}).RuneWidth(r))
	}
	return width
}

// Full viewport width for printed scrollback box rules, floored at thirty two columns
func scrollbackBoxWidth() int { return max(32, terminalColumns()) }
