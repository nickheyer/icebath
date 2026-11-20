package pystr

import (
	"math/big"
	"strings"
	"unicode"
)

// Base ten value int of a str gives, false where int raises ValueError
func IntText(s string) (*big.Int, bool) {
	t := Strip(s)
	neg := false
	if strings.HasPrefix(t, "+") || strings.HasPrefix(t, "-") {
		neg, t = t[0] == '-', t[1:]
	}
	var b strings.Builder
	prevDigit := false
	for _, r := range t {
		if r == '_' {
			if !prevDigit {
				return nil, false
			}
			prevDigit = false
			continue
		}
		if !unicode.Is(unicode.Nd, r) {
			return nil, false
		}
		b.WriteByte(byte('0' + decimalDigitValue(r)))
		prevDigit = true
	}
	if !prevDigit {
		return nil, false
	}
	n, ok := new(big.Int).SetString(b.String(), 10)
	if ok && neg {
		n.Neg(n)
	}
	return n, ok
}

// Digit value of a Unicode Nd code point, every Nd run being ten digits from zero
func decimalDigitValue(r rune) int {
	k := 0
	for unicode.Is(unicode.Nd, r-rune(k)-1) {
		k++
	}
	return k % 10
}
