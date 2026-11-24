package textutil

import (
	"strings"
	"unicode/utf8"
)

// s between runs of spaces that make it width characters long, the odd space going to the right; s itself when it is already that long
func Center(s string, width int) string {
	pad := width - utf8.RuneCountInString(s)
	if pad <= 0 {
		return s
	}
	left := pad / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", pad-left)
}
