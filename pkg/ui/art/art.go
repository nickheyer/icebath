// Wordmark and ornaments the interface decorates itself with
package art

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Wordmark in half blocks, three rows
var Wordmark = []string{
	"▀█▀ █▀▀ █▀▀ █▀▄ █▀█ ▀█▀ █ █",
	" █  █   █▀▀ █▀▄ █▀█  █  █▀█",
	"▄█▄ ▀▀▀ ▀▀▀ ▀▀  ▀ ▀  ▀  ▀ ▀",
}

// Wordmark for terminals without block characters
var WordmarkASCII = []string{
	" _ ___ ___ ___  _ _____ _ _ ",
	"| |/ __| __| _ )/_\\_   _| || |",
	"| | (__| _|| _ \\/ _ \\| | | __ |",
	"|_|\\___|___|___/_/ \\_\\_| |_||_|",
}

// Rows of the wordmark, the ASCII one when plain
func Mark(plain bool) []string {
	if plain {
		return WordmarkASCII
	}
	return Wordmark
}

// Widest row of the lines in cells
func Width(lines []string) int {
	w := 0
	for _, l := range lines {
		w = max(w, ansi.StringWidth(l))
	}
	return w
}

// Lines with every column painted in the color at its share of the sweep
func Paint(lines []string, sweep []color.Color) []string {
	if len(sweep) == 0 {
		return append([]string(nil), lines...)
	}
	width := max(Width(lines), 1)
	out := make([]string, len(lines))
	for i, l := range lines {
		var sb strings.Builder
		for x, r := range []rune(l) {
			if r == ' ' {
				sb.WriteRune(r)
				continue
			}
			sb.WriteString(lipgloss.NewStyle().Foreground(sweep[min(x*len(sweep)/width, len(sweep)-1)]).Render(string(r)))
		}
		out[i] = sb.String()
	}
	return out
}
