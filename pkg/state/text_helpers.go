package state

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
)

// str.lstrip with no argument
func lstrip(s string) string { return strings.TrimLeftFunc(s, unicode.IsSpace) }

// str.split with no argument
func splitWhitespace(s string) []string { return strings.FieldsFunc(s, unicode.IsSpace) }

// bytes.decode with utf-8 and errors replace, one replacement per maximal invalid subpart
func decodeUTF8Replace(b []byte) string {
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

// Python exception class and message a numeric conversion raises
type convError struct{ Class, Msg string }

// Binding refused because the session already belongs to another Telegram topic, a ValueError to class readers
type TopicAlreadyLinkedError struct{}

func (*TopicAlreadyLinkedError) Error() string {
	return "session is already linked to another Telegram topic"
}

func (*TopicAlreadyLinkedError) ErrorKind() string { return "ValueError" }

func (e *convError) Error() string { return e.Msg }

func (e *convError) ErrorKind() string { return e.Class }

// Decimal value of a Unicode digit, runs of ten starting at zero per the Unicode guarantee
func digitValue(r rune) (int, bool) {
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
func digitRun(s string) (string, bool) {
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
		d, ok := digitValue(r)
		if !ok {
			return "", false
		}
		out.WriteByte(byte('0' + d))
		prevDigit = true
	}
	return out.String(), prevDigit
}

// int of a decoded JSON value with the TypeError, ValueError and OverflowError int raises
func intOfValue(v any) (*big.Int, error) {
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
		f, err := floatOf(x)
		if err != nil {
			return nil, err
		}
		return intOfValue(f)
	case int:
		return big.NewInt(int64(x)), nil
	case int64:
		return big.NewInt(x), nil
	case float64:
		if math.IsNaN(x) {
			return nil, &convError{"ValueError", "not a number (NaN) has no whole number value"}
		}
		if math.IsInf(x, 0) {
			return nil, &convError{"OverflowError", "infinity has no whole number value"}
		}
		n, _ := big.NewFloat(math.Trunc(x)).Int(nil)
		return n, nil
	case string:
		body := strings.TrimSpace(x)
		sign := ""
		if body != "" && (body[0] == '+' || body[0] == '-') {
			sign, body = body[:1], body[1:]
		}
		if digits, ok := digitRun(body); ok {
			n, _ := new(big.Int).SetString(sign+digits, 10)
			return n, nil
		}
		return nil, &convError{"ValueError", strconv.Quote(x) + " is not a whole number"}
	}
	return nil, &convError{"TypeError", "expected a whole number or its text, got " + errchain.Kind(v)}
}

// float of a decoded JSON value with the TypeError, ValueError and OverflowError float raises
func floatOf(v any) (float64, error) {
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
				return 0, &convError{"OverflowError", "the number is too large for a decimal value"}
			}
			return f, nil
		}
		return floatOf(x.String())
	case string:
		if f, ok := parseFloat(x); ok {
			return f, nil
		}
		return 0, &convError{"ValueError", strconv.Quote(x) + " is not a number"}
	}
	return 0, &convError{"TypeError", "expected a number or its text, got " + errchain.Kind(v)}
}

// float of a string under the float constructor grammar, Unicode digits and digit underscores accepted
func parseFloat(s string) (float64, bool) {
	body := strings.TrimSpace(s)
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
			digits, ok := digitRun(part)
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
		digits, ok := digitRun(exponent)
		if !ok {
			return 0, false
		}
		text += "e" + expSign + digits
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// Lines of str.splitlines, breaking on every Python line boundary with CRLF as one break and no trailing empty line
func splitlines(s string) []string {
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
func intBase16(s string) (*big.Int, bool) {
	body := strings.TrimSpace(s)
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
			d, ok := digitValue(r)
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
