//go:build darwin || windows

package ziputil

import (
	"strings"
	"unicode/utf8"
)

// s with every byte that is not part of valid UTF-8 read as the Latin-1 character of the same value, for hosts whose file names must be valid Unicode
func validName(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(s)/2)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(rune(s[i]))
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}
