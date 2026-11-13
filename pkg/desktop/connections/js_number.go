package connections

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Characters String.prototype.trim removes
func jsSpace(r rune) bool {
	switch {
	case r >= '\t' && r <= '\r', r == ' ', r == 0xa0, r == 0x1680, r >= 0x2000 && r <= 0x200a,
		r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000, r == 0xfeff:
		return true
	}
	return false
}

// String.prototype.trim of text
func jsTrim(text string) string { return strings.TrimFunc(text, jsSpace) }

var (
	strDecimalLiteral = regexp.MustCompile(`^[+-]?(?:Infinity|[0-9]+\.?[0-9]*(?:[eE][+-]?[0-9]+)?|\.[0-9]+(?:[eE][+-]?[0-9]+)?)$`)
	strNonDecimal     = regexp.MustCompile(`^0(?:[xX]([0-9a-fA-F]+)|[oO]([0-7]+)|[bB]([01]+))$`)
)

// StringToNumber of the ECMAScript spec
func ecmaStringToNumber(s string) float64 {
	t := jsTrim(s)
	if t == "" {
		return 0
	}
	if m := strNonDecimal.FindStringSubmatch(t); m != nil {
		digits, base := m[1], 16
		if m[2] != "" {
			digits, base = m[2], 8
		} else if m[3] != "" {
			digits, base = m[3], 2
		}
		n, _ := new(big.Int).SetString(digits, base)
		f, _ := new(big.Float).SetInt(n).Float64()
		return f
	}
	if !strDecimalLiteral.MatchString(t) {
		return math.NaN()
	}
	switch strings.TrimLeft(t, "+-") {
	case "Infinity":
		if strings.HasPrefix(t, "-") {
			return math.Inf(-1)
		}
		return math.Inf(1)
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f
		}
		return math.NaN()
	}
	return f
}

// ToString of a decoded JSON value inside an Array join
func ecmaJoinElement(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return jsNumber(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = ecmaJoinElement(e)
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// Number(value) of a decoded JSON value
func ecmaToNumber(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return t
	case string:
		return ecmaStringToNumber(t)
	case []any:
		return ecmaStringToNumber(ecmaJoinElement(t))
	}
	return math.NaN()
}
