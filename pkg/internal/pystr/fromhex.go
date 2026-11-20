package pystr

import "fmt"

// Bytes of hex digit pairs with ASCII whitespace allowed between pairs, the ValueError of bytes fromhex otherwise
func BytesFromHex(s string) ([]byte, error) {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' }
	digit := func(c byte) int {
		switch {
		case c >= '0' && c <= '9':
			return int(c - '0')
		case c >= 'a' && c <= 'f':
			return int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			return int(c-'A') + 10
		}
		return 16
	}
	position := func(i int) int { return len([]rune(s[:i])) }
	out := make([]byte, 0, len(s)/2)
	for i := 0; i < len(s); {
		if isSpace(s[i]) {
			for i < len(s) && isSpace(s[i]) {
				i++
			}
			if i >= len(s) {
				break
			}
		}
		top := digit(s[i])
		if top >= 16 {
			return nil, fmt.Errorf("non-hexadecimal number found in fromhex() arg at position %d", position(i))
		}
		i++
		if i >= len(s) || digit(s[i]) >= 16 {
			return nil, fmt.Errorf("non-hexadecimal number found in fromhex() arg at position %d", position(i))
		}
		out = append(out, byte(top<<4|digit(s[i])))
		i++
	}
	return out, nil
}
