package updater

import (
	"math"
	"strconv"
	"strings"
)

// Number conversion of a decoded JSON value as JavaScript Number does it
func jsToNumber(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		return jsStringToNumber(x)
	case []any:
		switch len(x) {
		case 0:
			return 0
		case 1:
			return jsStringToNumber(jsString(x[0]))
		}
		return math.NaN()
	}
	return math.NaN()
}

// Number conversion of a string with JavaScript whitespace, hex, octal, binary and Infinity rules
func jsStringToNumber(s string) float64 {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0
	}
	switch t {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	for _, prefix := range []struct {
		p    string
		base int
	}{{"0x", 16}, {"0X", 16}, {"0o", 8}, {"0O", 8}, {"0b", 2}, {"0B", 2}} {
		if strings.HasPrefix(t, prefix.p) {
			n, err := strconv.ParseUint(t[2:], prefix.base, 64)
			if err != nil {
				return math.NaN()
			}
			return float64(n)
		}
	}
	for _, r := range t {
		if !(r >= '0' && r <= '9') && r != '.' && r != 'e' && r != 'E' && r != '+' && r != '-' {
			return math.NaN()
		}
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

// Truthiness of a decoded JSON value as JavaScript Boolean reads it
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int:
		return x != 0
	case string:
		return x != ""
	}
	return true
}

// String conversion of a decoded JSON value as JavaScript String does it
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(x)
	case string:
		return x
	case float64:
		return jsNumberText(x)
	case int:
		return strconv.Itoa(x)
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			if item != nil {
				parts[i] = jsString(item)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// Shortest round trip text of a number as JavaScript prints it
func jsNumberText(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	if math.Abs(f) >= 1e21 || math.Abs(f) < 1e-6 {
		text := strconv.FormatFloat(f, 'e', -1, 64)
		mantissa, exp, _ := strings.Cut(text, "e")
		if exp[0] == '+' {
			return mantissa + "e+" + strings.TrimLeft(exp[1:], "0")
		}
		return mantissa + "e-" + strings.TrimLeft(exp[1:], "0")
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// Whether a number is a safe JavaScript integer
func jsSafeInteger(f float64) bool {
	return !math.IsInf(f, 0) && !math.IsNaN(f) && math.Trunc(f) == f && math.Abs(f) <= 9007199254740991
}

// Whether a number is finite as JavaScript Number.isFinite reads it
func jsFinite(f float64) bool {
	return !math.IsInf(f, 0) && !math.IsNaN(f)
}
