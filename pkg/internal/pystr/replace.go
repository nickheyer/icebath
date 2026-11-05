package pystr

import "unicode/utf8"

// bytes.decode utf-8 with errors replace, one U+FFFD per maximal invalid subpart
func DecodeUTF8Replace(b []byte) string {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && n <= 1 {
			out = utf8.AppendRune(out, utf8.RuneError)
			i += UTF8MaximalSubpart(b[i:])
			continue
		}
		out = append(out, b[i:i+n]...)
		i += n
	}
	return string(out)
}
