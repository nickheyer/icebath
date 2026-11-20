package pystr

import (
	"strings"
	"unicode/utf8"
)

// str.ljust with spaces, padding on the right to width code points
func Ljust(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// str.rjust with spaces, padding on the left to width code points
func Rjust(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}

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
