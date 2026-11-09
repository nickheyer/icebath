package pystr

import (
	"bytes"
	"unicode/utf8"
)

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

// Decodes bytes as ascii with every byte above 127 replaced by U+FFFD
func DecodeASCIIReplace(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c < 0x80 {
			out = append(out, c)
		} else {
			out = utf8.AppendRune(out, utf8.RuneError)
		}
	}
	return string(out)
}

// Decodes utf-8-sig with errors ignore, the BOM stripped and every undecodable byte run dropped
func DecodeUTF8SigIgnore(b []byte) string {
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && n <= 1 {
			i += UTF8MaximalSubpart(b[i:])
			continue
		}
		out = append(out, b[i:i+n]...)
		i += n
	}
	return string(out)
}
