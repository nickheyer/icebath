package textutil

import (
	"strings"
	"unicode"
)

// Whether s is non-empty and holds nothing but the ASCII digits 0 to 9
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !isASCIIDigit(s[i]) {
			return false
		}
	}
	return true
}

func isASCIIDigit(c byte) bool { return '0' <= c && c <= '9' }

// s with each decimal digit of another script (Arabic-Indic, Devanagari, fullwidth and every other Unicode Nd digit) replaced by the ASCII digit of the same value, everything else kept
func ASCIIDigits(s string) string {
	if !strings.ContainsFunc(s, isNonASCIIDigit) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isNonASCIIDigit(r) {
			r = '0' + digitValue(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isNonASCIIDigit(r rune) bool { return r > '9' && unicode.IsDigit(r) }

// Value 0 to 9 of a decimal digit of any script, its offset in the run of digits around it counted in tens since Unicode encodes every script's digits as ten in a row from zero
func DigitValue(r rune) int { return int(digitValue(r)) }

func digitValue(r rune) rune {
	start := r
	for unicode.IsDigit(start - 1) {
		start--
	}
	return (r - start) % 10
}
