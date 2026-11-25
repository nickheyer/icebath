package textutil

import (
	"strconv"
	"strings"
)

// Whole number s writes in base ten, white space around it, a leading sign and single underscores between digits allowed; otherwise a *strconv.NumError, its ErrRange coming with the value clamped to the int64 bounds
func ParseInt(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if strings.IndexByte(t, '_') >= 0 {
		if t = dropDigitSeparators(t); t == "" {
			return 0, &strconv.NumError{Func: "ParseInt", Num: s, Err: strconv.ErrSyntax}
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return n, &strconv.NumError{Func: "ParseInt", Num: s, Err: err.(*strconv.NumError).Err}
	}
	return n, nil
}

// Characters of t with each underscore between two digits removed, empty when an underscore sits anywhere else
func dropDigitSeparators(t string) string {
	b := make([]byte, 0, len(t))
	for i := range len(t) {
		if t[i] != '_' {
			b = append(b, t[i])
			continue
		}
		if i == 0 || i == len(t)-1 || !isASCIIDigit(t[i-1]) || !isASCIIDigit(t[i+1]) {
			return ""
		}
	}
	return string(b)
}

// Truth value of a yes or no word: 1, true, yes or on and 0, false, no or off in any letter case, white space around ignored; ok is false for any other text
func ParseBool(s string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// Whole number s writes in base ten as ParseInt reads it, digits of any script taken for their values; false for anything else and for a value past the int64 range
func ParseIntAnyScript(s string) (int64, bool) {
	n, err := ParseInt(ASCIIDigits(s))
	return n, err == nil
}
