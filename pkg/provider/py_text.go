package provider

import (
	"math/big"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Decimal digit value of any Unicode Nd rune, every Nd run starts at a zero
func unicodeDigitValue(r rune) int {
	start := r
	for start > 0 && unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return int(r-start) % 10
}

// Exact integer text as Python int() reads it, signs, surrounding whitespace, single underscores between digits
func pyIntText(raw string) (int64, bool) {
	s := strings.TrimFunc(raw, unicode.IsSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	n := new(big.Int)
	ten := big.NewInt(10)
	prevDigit := false
	for _, r := range s {
		if r == '_' {
			if !prevDigit {
				return 0, false
			}
			prevDigit = false
			continue
		}
		if !unicode.Is(unicode.Nd, r) {
			return 0, false
		}
		n.Mul(n, ten).Add(n, big.NewInt(int64(unicodeDigitValue(r))))
		prevDigit = true
	}
	if !prevDigit {
		return 0, false
	}
	if neg {
		n.Neg(n)
	}
	if !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}

// Pads to the width in characters with trailing spaces
func pickerLjust(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// Pads to the width in characters with leading spaces
func pickerRjust(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}
