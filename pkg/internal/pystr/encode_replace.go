package pystr

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// UTF-8 bytes of s like str.encode with errors replace, each lone surrogate becoming a question mark
func EncodeUTF8Replace(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range Runes(s) {
		if IsSurrogate(r) {
			out = append(out, '?')
			continue
		}
		out = utf8.AppendRune(out, r)
	}
	return out
}

// UTF-8 text of s like str.encode with errors backslashreplace, each lone surrogate becoming a backslash u escape
func EncodeUTF8BackslashReplace(s string) string {
	var b strings.Builder
	for _, r := range Runes(s) {
		if IsSurrogate(r) {
			fmt.Fprintf(&b, "\\u%04x", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
