package state

import (
	"crypto/sha256"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Canonical text of a display key, the tuple notation the stored display identity hashes
type keyText struct{ b strings.Builder }

// Opens a tuple
func (k *keyText) open() { k.b.WriteByte('(') }

// Closes a tuple of n elements, a single element keeping its trailing comma
func (k *keyText) close(n int) {
	if n == 1 {
		k.b.WriteByte(',')
	}
	k.b.WriteByte(')')
}

// Separates two tuple elements
func (k *keyText) sep() { k.b.WriteString(", ") }

// Writes one stored cell, NULL, text, integer, real or blob
func (k *keyText) cell(v any) {
	switch t := v.(type) {
	case nil:
		k.b.WriteString("None")
	case string:
		k.str(t)
	case []byte:
		k.bytes(t)
	case int64:
		k.b.WriteString(strconv.FormatInt(t, 10))
	case int:
		k.b.WriteString(strconv.Itoa(t))
	case float64:
		k.b.WriteString(shortestFloatText(t))
	case bool:
		if t {
			k.b.WriteString("True")
		} else {
			k.b.WriteString("False")
		}
	default:
		k.b.WriteString("None")
	}
}

// Writes quoted text, single quotes unless the text holds one and no double quote
func (k *keyText) str(s string) {
	quote := byte('\'')
	if strings.IndexByte(s, '\'') >= 0 && strings.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	k.b.WriteByte(quote)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			k.b.WriteString(`\x` + hex2(uint32(s[i])))
			i++
			continue
		}
		i += size
		switch {
		case r == rune(quote) || r == '\\':
			k.b.WriteByte('\\')
			k.b.WriteRune(r)
		case r == '\t':
			k.b.WriteString(`\t`)
		case r == '\n':
			k.b.WriteString(`\n`)
		case r == '\r':
			k.b.WriteString(`\r`)
		case r < ' ' || r == 0x7f:
			k.b.WriteString(`\x` + hex2(uint32(r)))
		case r < 0x7f:
			k.b.WriteRune(r)
		case unicode.IsPrint(r):
			k.b.WriteRune(r)
		case r <= 0xff:
			k.b.WriteString(`\x` + hex2(uint32(r)))
		case r <= 0xffff:
			k.b.WriteString(`\u` + hexN(uint32(r), 4))
		default:
			k.b.WriteString(`\U` + hexN(uint32(r), 8))
		}
	}
	k.b.WriteByte(quote)
}

// Writes a blob as a byte literal
func (k *keyText) bytes(p []byte) {
	quote := byte('\'')
	hasSingle, hasDouble := false, false
	for _, c := range p {
		hasSingle = hasSingle || c == '\''
		hasDouble = hasDouble || c == '"'
	}
	if hasSingle && !hasDouble {
		quote = '"'
	}
	k.b.WriteByte('b')
	k.b.WriteByte(quote)
	for _, c := range p {
		switch {
		case c == quote || c == '\\':
			k.b.WriteByte('\\')
			k.b.WriteByte(c)
		case c == '\t':
			k.b.WriteString(`\t`)
		case c == '\n':
			k.b.WriteString(`\n`)
		case c == '\r':
			k.b.WriteString(`\r`)
		case c < ' ' || c >= 0x7f:
			k.b.WriteString(`\x` + hex2(uint32(c)))
		default:
			k.b.WriteByte(c)
		}
	}
	k.b.WriteByte(quote)
}

// Two lower case hex digits of v
func hex2(v uint32) string { return hexN(v, 2) }

// Lower case hex of v zero padded to n digits
func hexN(v uint32, n int) string {
	s := strconv.FormatUint(uint64(v), 16)
	if len(s) < n {
		s = strings.Repeat("0", n-len(s)) + s
	}
	return s
}

// Shortest decimal text that reads back as f, fixed notation between 1e-4 and 1e16 and exponent notation outside
func shortestFloatText(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	sign := ""
	if strings.HasPrefix(sci, "-") {
		sign, sci = "-", sci[1:]
	}
	mantissa, expText, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mantissa, ".", "", 1)
	point := exp + 1
	if point <= -4 || point > 16 {
		out := digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		expSign := "+"
		if exp < 0 {
			expSign, exp = "-", -exp
		}
		expDigits := strconv.Itoa(exp)
		if len(expDigits) < 2 {
			expDigits = "0" + expDigits
		}
		return sign + out + "e" + expSign + expDigits
	}
	switch {
	case point <= 0:
		return sign + "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		return sign + digits + strings.Repeat("0", point-len(digits)) + ".0"
	}
	return sign + digits[:point] + "." + digits[point:]
}

// Display key text of a row with stable tool call ids, the arguments a prune rewrites left out
func stableToolKeyText(timestamp, toolCallID any, callIDs []string, toolName any) string {
	var k keyText
	k.open()
	k.str("assistant")
	k.sep()
	k.cell(nil)
	k.sep()
	k.cell(timestamp)
	k.sep()
	k.cell(toolCallID)
	k.sep()
	k.open()
	for i, id := range callIDs {
		if i > 0 {
			k.sep()
		}
		k.str(id)
	}
	k.close(len(callIDs))
	k.sep()
	k.cell(toolName)
	k.close(6)
	return k.b.String()
}

// Display key text of a row keyed by its content
func contentKeyText(role string, content, timestamp, toolCallID, toolCalls, toolName any) string {
	var k keyText
	k.open()
	for i, v := range []any{role, content, timestamp, toolCallID, toolCalls, toolName} {
		if i > 0 {
			k.sep()
		}
		k.cell(v)
	}
	k.close(6)
	return k.b.String()
}

// Fixed width durable identity of a display key text
func displayIdentity(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}
