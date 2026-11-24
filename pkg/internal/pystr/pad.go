package pystr

import (
	"strings"
	"unicode/utf8"
)

// str.center with spaces, the odd extra space placed where CPython puts it
func Center(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	marg := width - n
	left := marg/2 + (marg & width & 1)
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", marg-left)
}
