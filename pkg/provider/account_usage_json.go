package provider

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// pyJSONParser decodes JSON the way Python json.loads does and records the Python repr of each value
type pyJSONParser struct {
	src []rune
	pos int
}

// Decodes a response body as Python json.loads does, returning the value and its Python repr
func pyJSONLoads(body []byte) (any, string, error) {
	text, err := pyJSONDecodeBytes(body)
	if err != nil {
		return nil, "", err
	}
	p := &pyJSONParser{src: text}
	p.skipSpace()
	v, repr, err := p.value()
	if err != nil {
		return nil, "", err
	}
	p.skipSpace()
	if p.pos < len(p.src) {
		return nil, "", fmt.Errorf("Extra data: char %d", p.pos)
	}
	return v, repr, nil
}

// Text of a JSON byte body by the json.detect_encoding rules, lone surrogates kept as surrogate runes
func pyJSONDecodeBytes(b []byte) ([]rune, error) {
	if bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
		return nil, errors.New("Unexpected UTF-8 BOM (decode using utf-8-sig)")
	}
	enc := "utf-8"
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE, 0, 0}) || bytes.HasPrefix(b, []byte{0, 0, 0xFE, 0xFF}):
		enc = "utf-32"
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}) || bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		enc = "utf-16"
	case len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0:
		enc = "utf-32-be"
	case len(b) >= 4 && b[0] == 0:
		enc = "utf-16-be"
	case len(b) >= 4 && b[1] == 0 && b[2] == 0 && b[3] == 0:
		enc = "utf-32-le"
	case len(b) >= 4 && b[1] == 0:
		enc = "utf-16-le"
	case len(b) == 2 && b[0] == 0:
		enc = "utf-16-be"
	case len(b) == 2 && b[1] == 0:
		enc = "utf-16-le"
	}
	switch enc {
	case "utf-8":
		if !utf8.Valid(b) {
			return nil, errors.New("'utf-8' codec can't decode the response body")
		}
		return []rune(string(b)), nil
	case "utf-32", "utf-32-be", "utf-32-le":
		var order binary.ByteOrder = binary.LittleEndian
		if enc == "utf-32-be" || (enc == "utf-32" && b[0] == 0) {
			order = binary.BigEndian
		}
		if enc == "utf-32" {
			b = b[4:]
		}
		if len(b)%4 != 0 {
			return nil, errors.New("'utf-32' codec can't decode the truncated response body")
		}
		out := make([]rune, 0, len(b)/4)
		for i := 0; i < len(b); i += 4 {
			r := order.Uint32(b[i:])
			if r > unicode.MaxRune || (r >= 0xD800 && r <= 0xDFFF) {
				return nil, errors.New("'utf-32' codec can't decode the response body")
			}
			out = append(out, rune(r))
		}
		return out, nil
	}
	var order binary.ByteOrder = binary.LittleEndian
	if enc == "utf-16-be" || (enc == "utf-16" && b[0] == 0xFE) {
		order = binary.BigEndian
	}
	if enc == "utf-16" {
		b = b[2:]
	}
	if len(b)%2 != 0 {
		return nil, errors.New("'utf-16' codec can't decode the truncated response body")
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = order.Uint16(b[2*i:])
	}
	for i := 0; i < len(units); i++ {
		if utf16.IsSurrogate(rune(units[i])) && (units[i] >= 0xDC00 || i+1 >= len(units) || units[i+1] < 0xDC00 || units[i+1] > 0xDFFF) {
			return nil, errors.New("'utf-16' codec can't decode the response body")
		}
		if utf16.IsSurrogate(rune(units[i])) {
			i++
		}
	}
	return utf16.Decode(units), nil
}

func (p *pyJSONParser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *pyJSONParser) errorf(msg string) error {
	return fmt.Errorf("%s: char %d", msg, p.pos)
}

func (p *pyJSONParser) hasPrefix(word string) bool {
	w := []rune(word)
	if p.pos+len(w) > len(p.src) {
		return false
	}
	for i, r := range w {
		if p.src[p.pos+i] != r {
			return false
		}
	}
	return true
}

// One JSON value and its Python repr, objects keep insertion order and the last duplicate value
func (p *pyJSONParser) value() (any, string, error) {
	if p.pos >= len(p.src) {
		return nil, "", p.errorf("Expecting value")
	}
	for _, lit := range []struct {
		word string
		v    any
		repr string
	}{{"null", nil, "None"}, {"true", true, "True"}, {"false", false, "False"},
		{"NaN", math.NaN(), "nan"}, {"Infinity", math.Inf(1), "inf"}, {"-Infinity", math.Inf(-1), "-inf"}} {
		if p.hasPrefix(lit.word) {
			p.pos += len(lit.word)
			return lit.v, lit.repr, nil
		}
	}
	switch c := p.src[p.pos]; {
	case c == '"':
		runes, err := p.str()
		if err != nil {
			return nil, "", err
		}
		return pyRunesString(runes), pyRunesRepr(runes), nil
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, "", p.errorf("Expecting value")
}

func (p *pyJSONParser) object() (any, string, error) {
	p.pos++
	out := map[string]any{}
	var keys []string
	reprs := map[string]string{}
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		return out, "{}", nil
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != '"' {
			return nil, "", p.errorf("Expecting property name enclosed in double quotes")
		}
		runes, err := p.str()
		if err != nil {
			return nil, "", err
		}
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, "", p.errorf("Expecting ':' delimiter")
		}
		p.pos++
		p.skipSpace()
		v, repr, err := p.value()
		if err != nil {
			return nil, "", err
		}
		key := pyRunesString(runes)
		if _, seen := out[key]; !seen {
			keys = append(keys, key)
			reprs[key+"\x00k"] = pyRunesRepr(runes)
		}
		out[key] = v
		reprs[key] = repr
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.pos < len(p.src) && p.src[p.pos] == '}' {
			p.pos++
			break
		}
		return nil, "", p.errorf("Expecting ',' delimiter")
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = reprs[k+"\x00k"] + ": " + reprs[k]
	}
	return out, "{" + strings.Join(parts, ", ") + "}", nil
}

func (p *pyJSONParser) array() (any, string, error) {
	p.pos++
	out := []any{}
	var parts []string
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		return out, "[]", nil
	}
	for {
		p.skipSpace()
		v, repr, err := p.value()
		if err != nil {
			return nil, "", err
		}
		out = append(out, v)
		parts = append(parts, repr)
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.pos < len(p.src) && p.src[p.pos] == ']' {
			p.pos++
			break
		}
		return nil, "", p.errorf("Expecting ',' delimiter")
	}
	return out, "[" + strings.Join(parts, ", ") + "]", nil
}

// Integer literals stay exact json.Number values, fractions and exponents read as Python floats
func (p *pyJSONParser) number() (any, string, error) {
	start := p.pos
	digits := func() int {
		n := 0
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
			n++
		}
		return n
	}
	if p.src[p.pos] == '-' {
		p.pos++
	}
	if p.pos < len(p.src) && p.src[p.pos] == '0' {
		p.pos++
	} else if digits() == 0 {
		p.pos = start
		return nil, "", p.errorf("Expecting value")
	}
	isFloat := false
	if p.pos+1 < len(p.src) && p.src[p.pos] == '.' && p.src[p.pos+1] >= '0' && p.src[p.pos+1] <= '9' {
		p.pos++
		digits()
		isFloat = true
	}
	if p.pos < len(p.src) && (p.src[p.pos] == 'e' || p.src[p.pos] == 'E') {
		save := p.pos
		p.pos++
		if p.pos < len(p.src) && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
			p.pos++
		}
		if digits() == 0 {
			p.pos = save
		} else {
			isFloat = true
		}
	}
	text := string(p.src[start:p.pos])
	if !isFloat {
		if text == "-0" {
			text = "0"
		}
		return json.Number(text), text, nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, "", p.errorf("Expecting value")
	}
	return f, pyFloatRepr(f), nil
}

// JSON string body as runes, a lone surrogate escape kept as its surrogate rune like Python
func (p *pyJSONParser) str() ([]rune, error) {
	p.pos++
	var out []rune
	for {
		if p.pos >= len(p.src) {
			return nil, p.errorf("Unterminated string starting at")
		}
		c := p.src[p.pos]
		switch {
		case c == '"':
			p.pos++
			return out, nil
		case c < 0x20:
			return nil, p.errorf("Invalid control character at")
		case c != '\\':
			out = append(out, c)
			p.pos++
			continue
		}
		p.pos++
		if p.pos >= len(p.src) {
			return nil, p.errorf("Unterminated string starting at")
		}
		esc := p.src[p.pos]
		p.pos++
		simple := map[rune]rune{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}
		if r, ok := simple[esc]; ok {
			out = append(out, r)
			continue
		}
		if esc != 'u' {
			return nil, p.errorf("Invalid \\escape")
		}
		u, ok := p.hex4()
		if !ok {
			return nil, p.errorf("Invalid \\uXXXX escape")
		}
		if u >= 0xD800 && u <= 0xDBFF && p.hasPrefix(`\u`) {
			save := p.pos
			p.pos += 2
			if lo, ok := p.hex4(); ok && lo >= 0xDC00 && lo <= 0xDFFF {
				out = append(out, utf16.DecodeRune(u, lo))
				continue
			}
			p.pos = save
		}
		out = append(out, u)
	}
}

func (p *pyJSONParser) hex4() (rune, bool) {
	if p.pos+4 > len(p.src) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(p.src[p.pos:p.pos+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	p.pos += 4
	return rune(v), true
}

// Go string of decoded runes, a lone surrogate becoming the replacement character
func pyRunesString(runes []rune) string {
	return string(runes)
}

// Python repr of a decoded string, lone surrogates written as their backslash u escape
func pyRunesRepr(runes []rune) string {
	text := string(runes)
	quote := '\''
	if strings.ContainsRune(text, '\'') && !strings.ContainsRune(text, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteRune(quote)
	for _, r := range runes {
		switch {
		case r >= 0xD800 && r <= 0xDFFF:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			inner := pyStringRepr(string(r))
			inner = inner[1 : len(inner)-1]
			if r == '\'' || r == '"' {
				if r == quote {
					inner = `\` + string(r)
				} else {
					inner = string(r)
				}
			}
			b.WriteString(inner)
		}
	}
	b.WriteRune(quote)
	return b.String()
}
