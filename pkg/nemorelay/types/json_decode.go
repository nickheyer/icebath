package types

import (
	"fmt"
	"strconv"
	"unicode/utf8"
)

// JSONSyntaxError is a parse failure carrying the serde_json message and position
type JSONSyntaxError struct {
	Message string
	Line    int
	Column  int
}

// Error renders the failure exactly as serde_json Error Display does
func (e *JSONSyntaxError) Error() string {
	if e.Line == 0 {
		return e.Message
	}
	return fmt.Sprintf("%s at line %d column %d", e.Message, e.Line, e.Column)
}

const (
	errEofList          = "EOF while parsing a list"
	errEofObject        = "EOF while parsing an object"
	errEofString        = "EOF while parsing a string"
	errEofValue         = "EOF while parsing a value"
	errExpectedColon    = "expected `:`"
	errExpectedListEnd  = "expected `,` or `]`"
	errExpectedObjEnd   = "expected `,` or `}`"
	errExpectedIdent    = "expected ident"
	errExpectedValue    = "expected value"
	errInvalidEscape    = "invalid escape"
	errInvalidNumber    = "invalid number"
	errNumberRange      = "number out of range"
	errInvalidCodePoint = "invalid unicode code point"
	errControlChar      = "control character (\\u0000-\\u001F) found while parsing a string"
	errKeyString        = "key must be a string"
	errLoneSurrogate    = "lone leading surrogate in hex escape"
	errTrailingComma    = "trailing comma"
	errTrailingChars    = "trailing characters"
	errHexEnd           = "unexpected end of hex escape"
	errRecursion        = "recursion limit exceeded"
)

// DecodeJSON parses bytes into a JSON value as serde_json from_slice does
func DecodeJSON(b []byte) (any, error) {
	p := &jsonParser{data: b, depth: 128}
	return p.document()
}

// DecodeJSONString parses text into a JSON value as serde_json from_str does
func DecodeJSONString(s string) (any, error) {
	return DecodeJSON([]byte(s))
}

// Parses JSON keeping object entries in source order
func decodeJSONOrdered(b []byte) (any, error) {
	p := &jsonParser{data: b, depth: 128, ordered: true}
	return p.document()
}

type jsonParser struct {
	data    []byte
	index   int
	depth   int
	ordered bool
}

func (p *jsonParser) document() (any, error) {
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	if _, ok := p.whitespace(); ok {
		return nil, p.peekError(errTrailingChars)
	}
	return v, nil
}

func (p *jsonParser) positionOf(i int) (int, int) {
	line := 1
	start := 0
	for j := 0; j < i && j < len(p.data); j++ {
		if p.data[j] == '\n' {
			line++
			start = j + 1
		}
	}
	return line, i - start
}

func (p *jsonParser) errorAt(msg string) error {
	line, col := p.positionOf(p.index)
	return &JSONSyntaxError{Message: msg, Line: line, Column: col}
}

func (p *jsonParser) peekError(msg string) error {
	i := p.index + 1
	if i > len(p.data) {
		i = len(p.data)
	}
	line, col := p.positionOf(i)
	return &JSONSyntaxError{Message: msg, Line: line, Column: col}
}

func (p *jsonParser) whitespace() (byte, bool) {
	for p.index < len(p.data) {
		switch c := p.data[p.index]; c {
		case ' ', '\n', '\t', '\r':
			p.index++
		default:
			return c, true
		}
	}
	return 0, false
}

func (p *jsonParser) peekOrNull() byte {
	if p.index < len(p.data) {
		return p.data[p.index]
	}
	return 0
}

func (p *jsonParser) value() (any, error) {
	c, ok := p.whitespace()
	if !ok {
		return nil, p.peekError(errEofValue)
	}
	switch {
	case c == 'n':
		p.index++
		return nil, p.ident("ull")
	case c == 't':
		p.index++
		return true, p.ident("rue")
	case c == 'f':
		p.index++
		return false, p.ident("alse")
	case c == '-':
		p.index++
		return p.number(false)
	case c >= '0' && c <= '9':
		return p.number(true)
	case c == '"':
		p.index++
		return p.str()
	case c == '[':
		p.depth--
		if p.depth == 0 {
			return nil, p.peekError(errRecursion)
		}
		p.index++
		v, err := p.array()
		p.depth++
		return v, err
	case c == '{':
		p.depth--
		if p.depth == 0 {
			return nil, p.peekError(errRecursion)
		}
		p.index++
		v, err := p.object()
		p.depth++
		return v, err
	}
	return nil, p.peekError(errExpectedValue)
}

func (p *jsonParser) ident(rest string) error {
	for i := 0; i < len(rest); i++ {
		if p.index >= len(p.data) {
			return p.errorAt(errEofValue)
		}
		c := p.data[p.index]
		p.index++
		if c != rest[i] {
			return p.errorAt(errExpectedIdent)
		}
	}
	return nil
}

func (p *jsonParser) array() (any, error) {
	out := []any{}
	first := true
	for {
		c, ok := p.whitespace()
		if !ok {
			return nil, p.peekError(errEofList)
		}
		if c == ']' {
			p.index++
			return out, nil
		}
		if first {
			first = false
		} else if c == ',' {
			p.index++
			c2, ok := p.whitespace()
			switch {
			case !ok:
				return nil, p.peekError(errEofValue)
			case c2 == ']':
				return nil, p.peekError(errTrailingComma)
			}
		} else {
			return nil, p.peekError(errExpectedListEnd)
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

func (p *jsonParser) object() (any, error) {
	var ordered orderedObject
	out := map[string]any{}
	first := true
	for {
		c, ok := p.whitespace()
		if !ok {
			return nil, p.peekError(errEofObject)
		}
		if c == '}' {
			p.index++
			if p.ordered {
				if ordered == nil {
					ordered = orderedObject{}
				}
				return ordered, nil
			}
			return out, nil
		}
		if !first {
			if c != ',' {
				return nil, p.peekError(errExpectedObjEnd)
			}
			p.index++
			c, ok = p.whitespace()
		}
		first = false
		switch {
		case !ok:
			return nil, p.peekError(errEofValue)
		case c == '}':
			return nil, p.peekError(errTrailingComma)
		case c != '"':
			return nil, p.peekError(errKeyString)
		}
		p.index++
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		c, ok = p.whitespace()
		if !ok {
			return nil, p.peekError(errEofObject)
		}
		if c != ':' {
			return nil, p.peekError(errExpectedColon)
		}
		p.index++
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		key := k.(string)
		if p.ordered {
			replaced := false
			for i := range ordered {
				if ordered[i].key == key {
					ordered[i].value = v
					replaced = true
				}
			}
			if !replaced {
				ordered = ordered.with(key, v)
			}
		} else {
			out[key] = v
		}
	}
}

// Parses a string body after the opening quote with serde_json validation
func (p *jsonParser) str() (any, error) {
	var buf []byte
	start := p.index
	for {
		for p.index < len(p.data) {
			c := p.data[p.index]
			if c == '"' || c == '\\' || c < 0x20 {
				break
			}
			p.index++
		}
		if p.index == len(p.data) {
			return nil, p.errorAt(errEofString)
		}
		switch p.data[p.index] {
		case '"':
			buf = append(buf, p.data[start:p.index]...)
			p.index++
			if !utf8.Valid(buf) {
				return nil, p.errorAt(errInvalidCodePoint)
			}
			return string(buf), nil
		case '\\':
			buf = append(buf, p.data[start:p.index]...)
			p.index++
			var err error
			buf, err = p.escape(buf)
			if err != nil {
				return nil, err
			}
			start = p.index
		default:
			p.index++
			return nil, p.errorAt(errControlChar)
		}
	}
}

func (p *jsonParser) escape(buf []byte) ([]byte, error) {
	if p.index >= len(p.data) {
		return nil, p.errorAt(errEofString)
	}
	c := p.data[p.index]
	p.index++
	switch c {
	case '"':
		return append(buf, '"'), nil
	case '\\':
		return append(buf, '\\'), nil
	case '/':
		return append(buf, '/'), nil
	case 'b':
		return append(buf, '\b'), nil
	case 'f':
		return append(buf, '\f'), nil
	case 'n':
		return append(buf, '\n'), nil
	case 'r':
		return append(buf, '\r'), nil
	case 't':
		return append(buf, '\t'), nil
	case 'u':
		return p.unicodeEscape(buf)
	}
	return nil, p.errorAt(errInvalidEscape)
}

func (p *jsonParser) hexEscape() (uint16, error) {
	if len(p.data)-p.index < 4 {
		p.index = len(p.data)
		return 0, p.errorAt(errEofString)
	}
	text := string(p.data[p.index : p.index+4])
	p.index += 4
	n, err := strconv.ParseUint(text, 16, 16)
	if err != nil || text[0] == '+' || text[0] == '-' {
		return 0, p.errorAt(errInvalidEscape)
	}
	return uint16(n), nil
}

func (p *jsonParser) unicodeEscape(buf []byte) ([]byte, error) {
	n, err := p.hexEscape()
	if err != nil {
		return nil, err
	}
	if n >= 0xDC00 && n <= 0xDFFF {
		return nil, p.errorAt(errLoneSurrogate)
	}
	if n < 0xD800 || n > 0xDBFF {
		return utf8.AppendRune(buf, rune(n)), nil
	}
	if p.index >= len(p.data) {
		return nil, p.errorAt(errEofString)
	}
	if p.data[p.index] != '\\' {
		p.index++
		return nil, p.errorAt(errHexEnd)
	}
	p.index++
	if p.index >= len(p.data) {
		return nil, p.errorAt(errEofString)
	}
	if p.data[p.index] != 'u' {
		p.index++
		return nil, p.errorAt(errHexEnd)
	}
	p.index++
	n2, err := p.hexEscape()
	if err != nil {
		return nil, err
	}
	if n2 < 0xDC00 || n2 > 0xDFFF {
		return nil, p.errorAt(errLoneSurrogate)
	}
	r := (rune(n-0xD800)<<10 | rune(n2-0xDC00)) + 0x10000
	return utf8.AppendRune(buf, r), nil
}

// Parses a number with the exact serde_json algorithm and result kinds
func (p *jsonParser) number(positive bool) (any, error) {
	if p.index >= len(p.data) {
		return nil, p.errorAt(errEofValue)
	}
	c := p.data[p.index]
	p.index++
	var significand uint64
	switch {
	case c == '0':
		if n := p.peekOrNull(); n >= '0' && n <= '9' {
			return nil, p.peekError(errInvalidNumber)
		}
	case c >= '1' && c <= '9':
		significand = uint64(c - '0')
		for {
			n := p.peekOrNull()
			if n < '0' || n > '9' {
				break
			}
			digit := uint64(n - '0')
			if significand >= 1844674407370955161 && (significand > 1844674407370955161 || digit > 5) {
				f, err := p.longInteger(positive, significand)
				if err != nil {
					return nil, err
				}
				return f, nil
			}
			p.index++
			significand = significand*10 + digit
		}
	default:
		return nil, p.errorAt(errInvalidNumber)
	}
	switch p.peekOrNull() {
	case '.':
		return p.decimal(positive, significand, 0)
	case 'e', 'E':
		return p.exponent(positive, significand, 0)
	}
	if positive {
		return significand, nil
	}
	neg := -int64(significand)
	if neg >= 0 {
		return -float64(significand), nil
	}
	return neg, nil
}

func (p *jsonParser) decimal(positive bool, significand uint64, before int32) (any, error) {
	p.index++
	var after int32
	for {
		n := p.peekOrNull()
		if n < '0' || n > '9' {
			break
		}
		digit := uint64(n - '0')
		if significand >= 1844674407370955161 && (significand > 1844674407370955161 || digit > 5) {
			for {
				n := p.peekOrNull()
				if n < '0' || n > '9' {
					break
				}
				p.index++
			}
			if n := p.peekOrNull(); n == 'e' || n == 'E' {
				return p.exponent(positive, significand, before+after)
			}
			return p.fromParts(positive, significand, before+after)
		}
		p.index++
		significand = significand*10 + digit
		after--
	}
	if after == 0 {
		if p.index < len(p.data) {
			return nil, p.peekError(errInvalidNumber)
		}
		return nil, p.peekError(errEofValue)
	}
	if n := p.peekOrNull(); n == 'e' || n == 'E' {
		return p.exponent(positive, significand, before+after)
	}
	return p.fromParts(positive, significand, before+after)
}

func (p *jsonParser) exponent(positive bool, significand uint64, starting int32) (any, error) {
	p.index++
	positiveExp := true
	switch p.peekOrNull() {
	case '+':
		p.index++
	case '-':
		p.index++
		positiveExp = false
	}
	if p.index >= len(p.data) {
		return nil, p.errorAt(errEofValue)
	}
	c := p.data[p.index]
	p.index++
	if c < '0' || c > '9' {
		return nil, p.errorAt(errInvalidNumber)
	}
	exp := int32(c - '0')
	for {
		n := p.peekOrNull()
		if n < '0' || n > '9' {
			break
		}
		p.index++
		digit := int32(n - '0')
		if exp >= 214748364 && (exp > 214748364 || digit > 7) {
			if significand != 0 && positiveExp {
				return nil, p.errorAt(errNumberRange)
			}
			for {
				n := p.peekOrNull()
				if n < '0' || n > '9' {
					break
				}
				p.index++
			}
			if positive {
				return 0.0, nil
			}
			return negativeZero(), nil
		}
		exp = exp*10 + digit
	}
	var final int32
	if positiveExp {
		final = saturatingAddI32(starting, exp)
	} else {
		final = saturatingSubI32(starting, exp)
	}
	return p.fromParts(positive, significand, final)
}

func (p *jsonParser) longInteger(positive bool, significand uint64) (any, error) {
	var exponent int32
	for {
		switch n := p.peekOrNull(); {
		case n >= '0' && n <= '9':
			p.index++
			exponent++
		case n == '.':
			return p.decimal(positive, significand, exponent)
		case n == 'e' || n == 'E':
			return p.exponent(positive, significand, exponent)
		default:
			return p.fromParts(positive, significand, exponent)
		}
	}
}

// Combines significand and decimal exponent exactly as serde_json without float_roundtrip
func (p *jsonParser) fromParts(positive bool, significand uint64, exponent int32) (any, error) {
	f := float64(significand)
	for {
		abs := exponent
		if abs < 0 {
			abs = -abs
		}
		if abs >= 0 && int(abs) < len(pow10Table) {
			pow := pow10Table[abs]
			if exponent >= 0 {
				f *= pow
				if f > maxFloat64 {
					return nil, p.errorAt(errNumberRange)
				}
			} else {
				f /= pow
			}
			break
		}
		if f == 0 {
			break
		}
		if exponent >= 0 {
			return nil, p.errorAt(errNumberRange)
		}
		f /= 1e308
		exponent += 308
	}
	if positive {
		return f, nil
	}
	return -f, nil
}

const maxFloat64 = 1.7976931348623157e308

func negativeZero() float64 {
	z := 0.0
	return -z
}

func saturatingAddI32(a, b int32) int32 {
	s := int64(a) + int64(b)
	if s > 2147483647 {
		return 2147483647
	}
	if s < -2147483648 {
		return -2147483648
	}
	return int32(s)
}

func saturatingSubI32(a, b int32) int32 {
	return saturatingAddI32(a, -b)
}

var pow10Table = func() [309]float64 {
	var t [309]float64
	for i := range t {
		v, _ := strconv.ParseFloat("1e"+strconv.Itoa(i), 64)
		t[i] = v
	}
	return t
}()
