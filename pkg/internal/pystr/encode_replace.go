package pystr

import "unicode/utf8"

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
