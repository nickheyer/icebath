package pystr

import (
	"bytes"
	"unicode/utf8"
)

// Text of utf-8-sig bytes, one leading BOM dropped, the strict UnicodeDecodeError on invalid UTF-8
func DecodeUTF8Sig(raw []byte) (string, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size == 1 {
			return "", UTF8DecodeError(raw, i)
		}
		i += size
	}
	return string(raw), nil
}

// Text of a whole utf-8-sig file read, a lone BOM start reading as empty
func DecodeUTF8SigFile(raw []byte) (string, error) {
	if len(raw) < 3 && bytes.HasPrefix([]byte{0xef, 0xbb, 0xbf}, raw) {
		return "", nil
	}
	return DecodeUTF8Sig(raw)
}
