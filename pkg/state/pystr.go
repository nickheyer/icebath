package state

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Whitespace as str.isspace sees it, the information separators included
func pyIsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r >= 0x1c && r <= 0x20, r == 0x85, r == 0xa0, r == 0x1680:
		return true
	case r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

// str.strip with no argument
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// str.lstrip with no argument
func pyLstrip(s string) string { return strings.TrimLeftFunc(s, pyIsSpace) }

// str.split with no argument
func pySplitWhitespace(s string) []string { return strings.FieldsFunc(s, pyIsSpace) }

// Code point length of a string the way len and SQLite LENGTH count it
func pyLen(s string) int { return utf8.RuneCountInString(s) }

// bytes.decode with utf-8 and errors replace, one replacement per maximal invalid subpart
func pyDecodeUTF8Replace(b []byte) string {
	var out strings.Builder
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			out.WriteByte(c)
			i++
			continue
		}
		size, lo, hi := 0, byte(0x80), byte(0xbf)
		switch {
		case c >= 0xc2 && c <= 0xdf:
			size = 2
		case c == 0xe0:
			size, lo = 3, 0xa0
		case c == 0xed:
			size, hi = 3, 0x9f
		case c >= 0xe1 && c <= 0xef:
			size = 3
		case c == 0xf0:
			size, lo = 4, 0x90
		case c == 0xf4:
			size, hi = 4, 0x8f
		case c >= 0xf1 && c <= 0xf3:
			size = 4
		}
		k := 1
		for ; size > 0 && k < size && i+k < len(b); k++ {
			if (k == 1 && (b[i+k] < lo || b[i+k] > hi)) || (k > 1 && (b[i+k] < 0x80 || b[i+k] > 0xbf)) {
				break
			}
		}
		if size > 0 && k == size {
			out.Write(b[i : i+size])
			i += size
			continue
		}
		out.WriteRune(utf8.RuneError)
		i += k
	}
	return out.String()
}

// repr of a bytes value
func pyBytesRepr(b []byte) string {
	quote := byte('\'')
	if strings.IndexByte(string(b), '\'') >= 0 && strings.IndexByte(string(b), '"') < 0 {
		quote = '"'
	}
	var out strings.Builder
	out.WriteString("b")
	out.WriteByte(quote)
	for _, c := range b {
		switch {
		case c == quote || c == '\\':
			out.WriteByte('\\')
			out.WriteByte(c)
		case c == '\t':
			out.WriteString(`\t`)
		case c == '\n':
			out.WriteString(`\n`)
		case c == '\r':
			out.WriteString(`\r`)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&out, `\x%02x`, c)
		default:
			out.WriteByte(c)
		}
	}
	out.WriteByte(quote)
	return out.String()
}

// str of a decoded JSON value, containers rendering their items with repr
func pyStr(v any) string {
	switch x := v.(type) {
	case []byte:
		return pyBytesRepr(x)
	case json.Number:
		return x.String()
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = pyReprValue(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case pyObject:
		parts := make([]string, len(x))
		for i, kv := range x {
			parts[i] = pyRepr(kv.Key) + ": " + pyReprValue(kv.Value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		obj := make(pyObject, len(keys))
		for i, k := range keys {
			obj[i] = pyKeyValue{k, x[k]}
		}
		return pyStr(obj)
	}
	return pyFormat(v)
}

// repr of a decoded JSON value
func pyReprValue(v any) string {
	if s, ok := v.(string); ok {
		return pyRepr(s)
	}
	return pyStr(v)
}

// Python exception class and message a numeric conversion raises
type pyConvError struct{ Class, Msg string }

func (e *pyConvError) Error() string { return e.Class + ": " + e.Msg }

func (e *pyConvError) PyTypeName() string { return e.Class }

// Decimal value of a Unicode digit, runs of ten starting at zero per the Unicode guarantee
func pyDigitValue(r rune) (int, bool) {
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}
	if !unicode.Is(unicode.Nd, r) {
		return 0, false
	}
	start := r
	for unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return int(r-start) % 10, true
}

// ASCII digits of a run of Unicode digits with single underscores between them, false on any other shape
func pyDigitRun(s string) (string, bool) {
	var out strings.Builder
	prevDigit := false
	for _, r := range s {
		if r == '_' {
			if !prevDigit {
				return "", false
			}
			prevDigit = false
			continue
		}
		d, ok := pyDigitValue(r)
		if !ok {
			return "", false
		}
		out.WriteByte(byte('0' + d))
		prevDigit = true
	}
	return out.String(), prevDigit
}

// int of a decoded JSON value with the TypeError, ValueError and OverflowError int raises
func pyIntOf(v any) (*big.Int, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewInt(1), nil
		}
		return big.NewInt(0), nil
	case json.Number:
		if n, ok := new(big.Int).SetString(x.String(), 10); ok {
			return n, nil
		}
		f, err := pyFloatOf(x)
		if err != nil {
			return nil, err
		}
		return pyIntOf(f)
	case int:
		return big.NewInt(int64(x)), nil
	case int64:
		return big.NewInt(x), nil
	case float64:
		if math.IsNaN(x) {
			return nil, &pyConvError{"ValueError", "cannot convert float NaN to integer"}
		}
		if math.IsInf(x, 0) {
			return nil, &pyConvError{"OverflowError", "cannot convert float infinity to integer"}
		}
		n, _ := big.NewFloat(math.Trunc(x)).Int(nil)
		return n, nil
	case string:
		body := pyStrip(x)
		sign := ""
		if body != "" && (body[0] == '+' || body[0] == '-') {
			sign, body = body[:1], body[1:]
		}
		if digits, ok := pyDigitRun(body); ok {
			n, _ := new(big.Int).SetString(sign+digits, 10)
			return n, nil
		}
		return nil, &pyConvError{"ValueError", "invalid literal for int() with base 10: " + pyRepr(x)}
	}
	return nil, &pyConvError{"TypeError", "int() argument must be a string, a bytes-like object or a real number"}
}

// float of a decoded JSON value with the TypeError, ValueError and OverflowError float raises
func pyFloatOf(v any) (float64, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case json.Number:
		if n, ok := new(big.Int).SetString(x.String(), 10); ok {
			f, _ := new(big.Float).SetInt(n).Float64()
			if math.IsInf(f, 0) {
				return 0, &pyConvError{"OverflowError", "int too large to convert to float"}
			}
			return f, nil
		}
		return pyFloatOf(x.String())
	case string:
		if f, ok := pyParseFloat(x); ok {
			return f, nil
		}
		return 0, &pyConvError{"ValueError", "could not convert string to float: " + pyRepr(x)}
	}
	return 0, &pyConvError{"TypeError", "float() argument must be a string or a real number"}
}

// float of a string under the float constructor grammar, Unicode digits and digit underscores accepted
func pyParseFloat(s string) (float64, bool) {
	body := pyStrip(s)
	sign := ""
	if body != "" && (body[0] == '+' || body[0] == '-') {
		sign, body = body[:1], body[1:]
	}
	switch strings.ToLower(body) {
	case "inf", "infinity":
		f, err := strconv.ParseFloat(sign+"inf", 64)
		return f, err == nil
	case "nan":
		return math.NaN(), true
	}
	mantissa, exponent := body, ""
	if i := strings.IndexAny(body, "eE"); i >= 0 {
		mantissa, exponent = body[:i], body[i+1:]
		if exponent == "" {
			return 0, false
		}
	}
	intPart, fracPart, hasDot := strings.Cut(mantissa, ".")
	if intPart == "" && fracPart == "" {
		return 0, false
	}
	text := sign
	for i, part := range []string{intPart, fracPart} {
		if part != "" {
			digits, ok := pyDigitRun(part)
			if !ok {
				return 0, false
			}
			text += digits
		}
		if i == 0 && hasDot {
			text += "."
		}
	}
	if exponent != "" {
		expSign := ""
		if exponent[0] == '+' || exponent[0] == '-' {
			expSign, exponent = exponent[:1], exponent[1:]
		}
		digits, ok := pyDigitRun(exponent)
		if !ok {
			return 0, false
		}
		text += "e" + expSign + digits
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && !strings.Contains(err.Error(), "value out of range") {
		return 0, false
	}
	return f, true
}

// Lines of str.splitlines, breaking on every Python line boundary with CRLF as one break and no trailing empty line
func pySplitlines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			lines = append(lines, s[start:i])
			i += size
			if r == '\r' && i < len(s) && s[i] == '\n' {
				i++
			}
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// int(s, 16) with whitespace, sign, an optional 0x prefix and single underscores between digits, false where int raises ValueError
func pyIntBase16(s string) (*big.Int, bool) {
	body := pyStrip(s)
	neg := false
	if body != "" && (body[0] == '+' || body[0] == '-') {
		neg, body = body[0] == '-', body[1:]
	}
	prefixed := len(body) >= 2 && body[0] == '0' && (body[1] == 'x' || body[1] == 'X')
	if prefixed {
		body = strings.TrimPrefix(body[2:], "_")
	}
	var digits strings.Builder
	prevDigit := false
	for _, r := range body {
		if r == '_' {
			if !prevDigit {
				return nil, false
			}
			prevDigit = false
			continue
		}
		switch {
		case r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
			digits.WriteRune(r)
		default:
			d, ok := pyDigitValue(r)
			if !ok {
				return nil, false
			}
			digits.WriteByte(byte('0' + d))
		}
		prevDigit = true
	}
	if !prevDigit {
		return nil, false
	}
	n, _ := new(big.Int).SetString(digits.String(), 16)
	if neg {
		n.Neg(n)
	}
	return n, true
}
