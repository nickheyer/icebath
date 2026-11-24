package base64util

import "encoding/base64"

// Bytes of base64 text written loosely: the standard and URL safe alphabets alike, padding optional or in excess, ASCII whitespace anywhere ignored, and padding closing a quantum so base64 texts joined end to end decode as one; any other byte is a base64.CorruptInputError at its offset
func Decode(s string) ([]byte, error) {
	out := make([]byte, 0, len(s)/4*3+3)
	var acc uint32
	n, last := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var v byte
		switch {
		case c >= 'A' && c <= 'Z':
			v = c - 'A'
		case c >= 'a' && c <= 'z':
			v = c - 'a' + 26
		case c >= '0' && c <= '9':
			v = c - '0' + 52
		case c == '+' || c == '-':
			v = 62
		case c == '/' || c == '_':
			v = 63
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			continue
		case c == '=':
			if n == 1 {
				return nil, base64.CorruptInputError(last)
			}
			out, acc, n = flush(out, acc, n), 0, 0
			continue
		default:
			return nil, base64.CorruptInputError(i)
		}
		acc, n, last = acc<<6|uint32(v), n+1, i
		if n == 4 {
			out, acc, n = append(out, byte(acc>>16), byte(acc>>8), byte(acc)), 0, 0
		}
	}
	if n == 1 {
		return nil, base64.CorruptInputError(last)
	}
	return flush(out, acc, n), nil
}

// Out with the bytes a partial quantum of two or three characters holds appended
func flush(out []byte, acc uint32, n int) []byte {
	switch n {
	case 2:
		return append(out, byte(acc>>4))
	case 3:
		return append(out, byte(acc>>10), byte(acc>>2))
	}
	return out
}
