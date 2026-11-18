package tools

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var pyFloatTextRe = regexp.MustCompile(`\A[+-]?(?:(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:e[+-]?[0-9]+)?|inf|infinity|nan)\z`)

// Float text the way float() parses it, surrounding whitespace and Unicode digits allowed and no hex form
func pyFloatFromString(text string) (float64, error) {
	var b strings.Builder
	for _, r := range strings.TrimFunc(text, pyIsSpace) {
		if unicode.IsDigit(r) && r > 0x7f {
			start := r
			for unicode.IsDigit(start - 1) {
				start--
			}
			r = '0' + (r-start)%10
		}
		b.WriteRune(unicode.ToLower(r))
	}
	s := b.String()
	if !pyFloatTextRe.MatchString(s) {
		return 0, fmt.Errorf("%q is not a number", text)
	}
	switch strings.TrimLeft(s, "+-") {
	case "inf", "infinity":
		if strings.HasPrefix(s, "-") {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	case "nan":
		return math.NaN(), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, err
	}
	return f, nil
}
