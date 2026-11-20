package pyint

import (
	"math/big"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python int of a string in base 2 to 36 with its prefix, underscores and Unicode decimal digits
func ParseBase(text string, base int) (*big.Int, bool) {
	if base < 2 || base > 36 {
		return nil, false
	}
	text = pystr.Strip(text)
	sign := ""
	if strings.HasPrefix(text, "+") || strings.HasPrefix(text, "-") {
		sign, text = text[:1], text[1:]
	}
	prefixes := map[int]string{2: "0b", 8: "0o", 16: "0x"}
	previousUnderscore := true
	if p, ok := prefixes[base]; ok && len(text) >= 2 && strings.EqualFold(text[:2], p) {
		text = text[2:]
		previousUnderscore = false
	}
	var digits strings.Builder
	count := 0
	for _, r := range text {
		if r == '_' {
			if previousUnderscore {
				return nil, false
			}
			previousUnderscore = true
			continue
		}
		value := -1
		switch {
		case unicode.Is(unicode.Nd, r):
			start := r
			for unicode.Is(unicode.Nd, start-1) {
				start--
			}
			value = int(r-start) % 10
		case r >= 'a' && r <= 'z':
			value = int(r-'a') + 10
		case r >= 'A' && r <= 'Z':
			value = int(r-'A') + 10
		}
		if value < 0 || value >= base {
			return nil, false
		}
		digits.WriteByte("0123456789abcdefghijklmnopqrstuvwxyz"[value])
		previousUnderscore = false
		count++
	}
	if count == 0 || previousUnderscore {
		return nil, false
	}
	return new(big.Int).SetString(sign+digits.String(), base)
}
