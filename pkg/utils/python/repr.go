package pythonutil

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Whether a character appears as itself in a Python string literal, as opposed to an escape
func isPrintable(r rune) bool { return unicode.IsPrint(r) }

// s as a Python string literal, single quoted unless only double quotes spare an escape, escaped as Python's repr writes it; a byte that is not UTF-8 becomes the \udcXX escape Python's surrogateescape gives it, so the literal encodes back to the same bytes as a file name or argument, as the embedded interpreter receives such a string
func Quote(s string) string {
	return string(appendQuote(make([]byte, 0, len(s)+2), s))
}

func appendQuote(b []byte, s string) []byte {
	quote := byte('\'')
	if strings.IndexByte(s, '\'') >= 0 && strings.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	b = append(b, quote)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = appendHexEscape(b, 'u', 0xdc00+rune(s[i]), 4)
			i++
			continue
		}
		i += size
		switch {
		case r == rune(quote) || r == '\\':
			b = append(b, '\\', byte(r))
		case r == '\t':
			b = append(b, `\t`...)
		case r == '\n':
			b = append(b, `\n`...)
		case r == '\r':
			b = append(b, `\r`...)
		case r < 0x20 || r == 0x7f:
			b = appendHexEscape(b, 'x', r, 2)
		case r < 0x7f || isPrintable(r):
			b = utf8.AppendRune(b, r)
		case r < 0x100:
			b = appendHexEscape(b, 'x', r, 2)
		case r < 0x10000:
			b = appendHexEscape(b, 'u', r, 4)
		default:
			b = appendHexEscape(b, 'U', r, 8)
		}
	}
	return append(b, quote)
}

// Appends a backslash escape of the letter and the code point in lower case hex padded to width
func appendHexEscape(b []byte, letter byte, r rune, width int) []byte {
	b = append(b, '\\', letter)
	for shift := (width - 1) * 4; shift >= 0; shift -= 4 {
		b = append(b, "0123456789abcdef"[(r>>shift)&0xf])
	}
	return b
}

// Bytes as a Python bytes literal with the escapes Python's repr writes
func appendBytesLiteral(b []byte, p []byte) []byte {
	quote := byte('\'')
	if containsByte(p, '\'') && !containsByte(p, '"') {
		quote = '"'
	}
	b = append(b, 'b', quote)
	for _, c := range p {
		switch {
		case c == quote || c == '\\':
			b = append(b, '\\', c)
		case c == '\t':
			b = append(b, `\t`...)
		case c == '\n':
			b = append(b, `\n`...)
		case c == '\r':
			b = append(b, `\r`...)
		case c < 0x20 || c >= 0x7f:
			b = appendHexEscape(b, 'x', rune(c), 2)
		default:
			b = append(b, c)
		}
	}
	return append(b, quote)
}

func containsByte(p []byte, c byte) bool {
	for _, x := range p {
		if x == c {
			return true
		}
	}
	return false
}

// Shortest digits that read back to f, fixed point from 1e-4 up to 1e16 and exponent form outside, always with a fraction or exponent
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	digits := strconv.FormatFloat(math.Abs(f), 'e', -1, 64)
	mant, expText, _ := strings.Cut(digits, "e")
	exp, _ := strconv.Atoi(expText)
	mant = strings.Replace(mant, ".", "", 1)
	sign := ""
	if f < 0 {
		sign = "-"
	}
	if exp < -4 || exp >= 16 {
		out := mant[:1]
		if len(mant) > 1 {
			out += "." + mant[1:]
		}
		esign := "+"
		if exp < 0 {
			esign, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, esign, exp)
	}
	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + mant
	}
	if len(mant) <= exp+1 {
		return sign + mant + strings.Repeat("0", exp+1-len(mant)) + ".0"
	}
	return sign + mant[:exp+1] + "." + mant[exp+1:]
}

// A complex number as Python writes it: the bare imaginary part for a zero real part, else both in parentheses
func formatComplex(c complex128) string {
	re, im := real(c), imag(c)
	if re == 0 && !math.Signbit(re) {
		return complexPart(im) + "j"
	}
	ims := complexPart(im)
	if math.IsNaN(im) {
		ims = "+nan"
	} else if !strings.HasPrefix(ims, "-") {
		ims = "+" + ims
	}
	return "(" + complexPart(re) + ims + "j)"
}

func complexPart(f float64) string { return strings.TrimSuffix(formatFloat(f), ".0") }

// Overflowing literal that reads back as infinity
const infLiteral = "1e309"

// Number text with infinity and NaN spelled as expressions that evaluate to them, a NaN imaginary part as the difference of two infinite imaginary literals
func sourceNumber(s string) string {
	if strings.Contains(s, "nanj") {
		s = strings.ReplaceAll(s, "nanj", "("+infLiteral+"j-"+infLiteral+"j)")
	}
	if strings.Contains(s, "inf") {
		s = strings.ReplaceAll(s, "inf", infLiteral)
	}
	if strings.Contains(s, "nan") {
		s = strings.ReplaceAll(s, "nan", "("+infLiteral+"-"+infLiteral+")")
	}
	return s
}

// Python source text of a literal value: a Constant value of a tree or a value LiteralEval gives
func Literal(v any) string { return string(appendLiteral(nil, v)) }

// Text Python's repr gives a constant, the form its messages quote: Literal's text except that infinite and NaN numbers read inf and nan and the ellipsis reads Ellipsis
func constantRepr(v any) string {
	switch x := v.(type) {
	case float64:
		return formatFloat(x)
	case complex128:
		return formatComplex(x)
	case Ellipsis:
		return "Ellipsis"
	}
	return Literal(v)
}

func appendLiteral(b []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(b, "None"...)
	case bool:
		if x {
			return append(b, "True"...)
		}
		return append(b, "False"...)
	case int64:
		return strconv.AppendInt(b, x, 10)
	case int:
		return strconv.AppendInt(b, int64(x), 10)
	case *big.Int:
		return x.Append(b, 10)
	case float64:
		return append(b, sourceNumber(formatFloat(x))...)
	case complex128:
		return append(b, sourceNumber(formatComplex(x))...)
	case string:
		return appendQuote(b, x)
	case []byte:
		return appendBytesLiteral(b, x)
	case Ellipsis:
		return append(b, "..."...)
	case Tuple:
		b = append(b, '(')
		b = appendItems(b, x)
		if len(x) == 1 {
			b = append(b, ',')
		}
		return append(b, ')')
	case []any:
		b = append(b, '[')
		return append(appendItems(b, x), ']')
	case Set:
		if len(x) == 0 {
			return append(b, "set()"...)
		}
		b = append(b, '{')
		return append(appendItems(b, x), '}')
	case *Dict:
		b = append(b, '{')
		for i := range x.Keys {
			if i > 0 {
				b = append(b, ", "...)
			}
			b = appendLiteral(b, x.Keys[i])
			b = append(b, ": "...)
			b = appendLiteral(b, x.Values[i])
		}
		return append(b, '}')
	}
	return fmt.Append(b, v)
}

func appendItems(b []byte, items []any) []byte {
	for i, it := range items {
		if i > 0 {
			b = append(b, ", "...)
		}
		b = appendLiteral(b, it)
	}
	return b
}
