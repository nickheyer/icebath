package wire

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Deepest nesting the lenient JSON reader follows before it calls a text unreadable
const looseMaxDepth = 10000

// Spelling of re-encoded JSON text, the zero layout being compact with non-ASCII characters escaped
type TextLayout struct {
	Spaced bool
	Sorted bool
	UTF8   bool
}

// Reader of the JSON dialect models emit, accepting NaN and Infinity and, unless strict, raw control characters
type looseParser struct {
	s      string
	i      int
	strict bool
	emit   bool
	layout TextLayout
	depth  int
}

// Reports whether text is one JSON value in the lenient dialect
func looseValid(text string, strict bool) bool {
	p := looseParser{s: text, strict: strict}
	_, ok := p.document(nil)
	return ok
}

// Re-encodes lenient JSON in a layout, a repeated key keeping its first position and its last entry
func FormatJSON(text []byte, l TextLayout) (string, bool) {
	p := looseParser{s: string(text), emit: true, layout: l}
	out, ok := p.document(make([]byte, 0, len(text)))
	return string(out), ok
}

// Reads the whole text as one element surrounded by optional whitespace
func (p *looseParser) document(dst []byte) ([]byte, bool) {
	p.space()
	dst, ok := p.value(dst)
	p.space()
	return dst, ok && p.i == len(p.s)
}

// Skips JSON whitespace
func (p *looseParser) space() {
	for p.i < len(p.s) && isJSONSpace(p.s[p.i]) {
		p.i++
	}
}

// Consumes a literal when the text continues with it
func (p *looseParser) word(dst []byte, word string) ([]byte, bool) {
	if !strings.HasPrefix(p.s[p.i:], word) {
		return dst, false
	}
	p.i += len(word)
	if p.emit {
		dst = append(dst, word...)
	}
	return dst, true
}

// Reads one JSON element of any kind
func (p *looseParser) value(dst []byte) ([]byte, bool) {
	if p.i >= len(p.s) {
		return dst, false
	}
	switch c := p.s[p.i]; {
	case c == '"':
		return p.str(dst)
	case c == '{':
		return p.object(dst)
	case c == '[':
		return p.array(dst)
	case c == 'n':
		return p.word(dst, "null")
	case c == 't':
		return p.word(dst, "true")
	case c == 'f':
		return p.word(dst, "false")
	case c == 'N':
		return p.word(dst, "NaN")
	case c == 'I':
		return p.word(dst, "Infinity")
	}
	if out, ok := p.number(dst); ok {
		return out, true
	}
	return p.word(dst, "-Infinity")
}

// Reads a numeric literal, writing integers as their digits and the rest in shortest round-trip form
func (p *looseParser) number(dst []byte) ([]byte, bool) {
	start, i := p.i, p.i
	digits := func() int {
		n := 0
		for i < len(p.s) && p.s[i] >= '0' && p.s[i] <= '9' {
			i++
			n++
		}
		return n
	}
	if i < len(p.s) && p.s[i] == '-' {
		i++
	}
	switch {
	case i < len(p.s) && p.s[i] == '0':
		i++
	case digits() == 0:
		return dst, false
	}
	whole, real := i, false
	if i < len(p.s) && p.s[i] == '.' {
		i++
		if digits() > 0 {
			whole, real = i, true
		}
		i = whole
	}
	if i < len(p.s) && (p.s[i] == 'e' || p.s[i] == 'E') {
		i++
		if i < len(p.s) && (p.s[i] == '+' || p.s[i] == '-') {
			i++
		}
		if digits() > 0 {
			whole, real = i, true
		}
		i = whole
	}
	p.i = i
	if !p.emit {
		return dst, true
	}
	text := p.s[start:i]
	switch {
	case real:
		f, _ := strconv.ParseFloat(text, 64)
		dst = appendFloat(dst, f)
	case text == "-0":
		dst = append(dst, '0')
	default:
		dst = append(dst, text...)
	}
	return dst, true
}

// Appends a float in its shortest form that reads back exactly, always with a fraction or an exponent
func appendFloat(dst []byte, f float64) []byte {
	switch {
	case math.IsNaN(f):
		return append(dst, "NaN"...)
	case math.IsInf(f, 1):
		return append(dst, "Infinity"...)
	case math.IsInf(f, -1):
		return append(dst, "-Infinity"...)
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	if sci[0] == '-' {
		dst, sci = append(dst, '-'), sci[1:]
	}
	mantissa, exponent, _ := strings.Cut(sci, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	exp, _ := strconv.Atoi(exponent)
	point := exp + 1
	switch {
	case point > 16 || point < -3:
		dst = append(dst, digits[0])
		if len(digits) > 1 {
			dst = append(append(dst, '.'), digits[1:]...)
		}
		dst = append(dst, 'e', exponent[0])
		return append(dst, exponent[1:]...)
	case point <= 0:
		dst = append(dst, '0', '.')
		dst = append(dst, strings.Repeat("0", -point)...)
		return append(dst, digits...)
	case point >= len(digits):
		dst = append(dst, digits...)
		dst = append(dst, strings.Repeat("0", point-len(digits))...)
		return append(dst, '.', '0')
	}
	dst = append(dst, digits[:point]...)
	return append(append(dst, '.'), digits[point:]...)
}

// Appends one decoded character of a string, escaped as the layout asks
func (p *looseParser) char(dst []byte, r rune) []byte {
	switch {
	case r == '"' || r == '\\':
		return append(dst, '\\', byte(r))
	case r == '\n':
		return append(dst, '\\', 'n')
	case r == '\r':
		return append(dst, '\\', 'r')
	case r == '\t':
		return append(dst, '\\', 't')
	case r == '\b':
		return append(dst, '\\', 'b')
	case r == '\f':
		return append(dst, '\\', 'f')
	case r >= 0x20 && r < 0x7f:
		return append(dst, byte(r))
	case r >= 0x7f && p.layout.UTF8:
		return utf8.AppendRune(dst, r)
	}
	return appendEscapedRune(dst, r)
}

// Number four hex digits at position i spell, false when they are not all hex
func (p *looseParser) hex4(i int) (rune, bool) {
	if i+4 > len(p.s) {
		return 0, false
	}
	v, err := strconv.ParseUint(p.s[i:i+4], 16, 16)
	return rune(v), err == nil
}

// Reads a string, pairing surrogate escapes and keeping a lone one as its escape
func (p *looseParser) str(dst []byte) ([]byte, bool) {
	p.i++
	if p.emit {
		dst = append(dst, '"')
	}
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			if p.emit {
				dst = append(dst, '"')
			}
			return dst, true
		case c == '\\':
			if p.i+1 >= len(p.s) {
				return dst, false
			}
			esc := p.s[p.i+1]
			p.i += 2
			var r rune
			switch esc {
			case '"', '\\', '/':
				r = rune(esc)
			case 'b':
				r = '\b'
			case 'f':
				r = '\f'
			case 'n':
				r = '\n'
			case 'r':
				r = '\r'
			case 't':
				r = '\t'
			case 'u':
				unit, ok := p.hex4(p.i)
				if !ok {
					return dst, false
				}
				p.i += 4
				r = unit
				if unit >= 0xd800 && unit <= 0xdbff && strings.HasPrefix(p.s[p.i:], `\u`) {
					if low, ok := p.hex4(p.i + 2); ok && low >= 0xdc00 && low <= 0xdfff {
						r = 0x10000 + (unit-0xd800)<<10 + (low - 0xdc00)
						p.i += 6
					}
				}
				if r >= 0xd800 && r <= 0xdfff {
					if p.emit {
						dst = appendHex4(dst, r)
					}
					continue
				}
			default:
				return dst, false
			}
			if p.emit {
				dst = p.char(dst, r)
			}
		case c < 0x20:
			if p.strict {
				return dst, false
			}
			p.i++
			if p.emit {
				dst = p.char(dst, rune(c))
			}
		default:
			r, size := utf8.DecodeRuneInString(p.s[p.i:])
			p.i += size
			if p.emit {
				dst = p.char(dst, r)
			}
		}
	}
	return dst, false
}

// Appends the separator between entries
func (p *looseParser) comma(dst []byte) []byte {
	if p.layout.Spaced {
		return append(dst, ',', ' ')
	}
	return append(dst, ',')
}

// Reads a bracketed list
func (p *looseParser) array(dst []byte) ([]byte, bool) {
	if p.depth++; p.depth > looseMaxDepth {
		return dst, false
	}
	defer func() { p.depth-- }()
	p.i++
	if p.emit {
		dst = append(dst, '[')
	}
	p.space()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		if p.emit {
			dst = append(dst, ']')
		}
		return dst, true
	}
	for {
		var ok bool
		if dst, ok = p.value(dst); !ok {
			return dst, false
		}
		p.space()
		if p.i >= len(p.s) {
			return dst, false
		}
		switch p.s[p.i] {
		case ']':
			p.i++
			if p.emit {
				dst = append(dst, ']')
			}
			return dst, true
		case ',':
			p.i++
			if p.emit {
				dst = p.comma(dst)
			}
			p.space()
		default:
			return dst, false
		}
	}
}

// Reads a braced dictionary, a repeated key keeping its first position and taking the last entry
func (p *looseParser) object(dst []byte) ([]byte, bool) {
	if p.depth++; p.depth > looseMaxDepth {
		return dst, false
	}
	defer func() { p.depth-- }()
	p.i++
	p.space()
	var entries [][2]string
	index := map[string]int{}
	for first := true; ; first = false {
		if first && p.i < len(p.s) && p.s[p.i] == '}' {
			break
		}
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return dst, false
		}
		key, ok := p.str(nil)
		if !ok {
			return dst, false
		}
		p.space()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return dst, false
		}
		p.i++
		p.space()
		value, ok := p.value(nil)
		if !ok {
			return dst, false
		}
		if p.emit {
			at, dup := index[string(key)]
			if !dup {
				at, entries = len(entries), append(entries, [2]string{string(key)})
				index[string(key)] = at
			}
			entries[at][1] = string(value)
		}
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
			p.space()
			continue
		}
		break
	}
	if p.i >= len(p.s) || p.s[p.i] != '}' {
		return dst, false
	}
	p.i++
	if !p.emit {
		return dst, true
	}
	if p.layout.Sorted {
		slices.SortStableFunc(entries, func(a, b [2]string) int {
			var x, y string
			if json.Unmarshal([]byte(a[0]), &x) != nil || json.Unmarshal([]byte(b[0]), &y) != nil {
				return strings.Compare(a[0], b[0])
			}
			return strings.Compare(x, y)
		})
	}
	dst = append(dst, '{')
	for n, e := range entries {
		if n > 0 {
			dst = p.comma(dst)
		}
		dst = append(append(dst, e[0]...), ':')
		if p.layout.Spaced {
			dst = append(dst, ' ')
		}
		dst = append(dst, e[1]...)
	}
	return append(dst, '}'), true
}
