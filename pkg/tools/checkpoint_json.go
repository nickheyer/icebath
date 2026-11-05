package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Python json.dumps text of a value, ASCII escaped with invalid path bytes written as surrogate escapes
func checkpointJSONEncode(v any) (string, error) {
	var b strings.Builder
	if err := checkpointEncodeValue(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Writes one value with the default json.dumps separators and sorted object keys
func checkpointEncodeValue(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case float64:
		switch {
		case math.IsNaN(x):
			b.WriteString("NaN")
		case math.IsInf(x, 1):
			b.WriteString("Infinity")
		case math.IsInf(x, -1):
			b.WriteString("-Infinity")
		default:
			b.WriteString(pyFloatRepr(x))
		}
	case json.Number:
		b.WriteString(x.String())
	case string:
		writeASCIIJSONString(b, x)
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		return checkpointEncodeValue(b, items)
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := checkpointEncodeValue(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writeASCIIJSONString(b, k)
			b.WriteString(": ")
			if err := checkpointEncodeValue(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return &checkpointValueError{msg: fmt.Sprintf("Object of type %T is not JSON serializable", v)}
	}
	return nil
}

// Strict json.loads over checkpoint files, numbers kept exact and surrogate escaped bytes restored
type checkpointJSONParser struct {
	s string
	i int
}

// Decodes a JSON document the way json.loads reads it, restoring surrogate escaped path bytes
func checkpointJSONDecode(text string) (any, error) {
	p := &checkpointJSONParser{s: text}
	p.ws()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, p.fail("Extra data")
	}
	return v, nil
}

func (p *checkpointJSONParser) fail(msg string) error {
	return &checkpointValueError{msg: fmt.Sprintf("%s: char %d", msg, p.i)}
}

func (p *checkpointJSONParser) ws() {
	for p.i < len(p.s) && strings.IndexByte(" \t\n\r", p.s[p.i]) >= 0 {
		p.i++
	}
}

// Consumes a literal word when it is next in the input
func (p *checkpointJSONParser) word(w string) bool {
	if strings.HasPrefix(p.s[p.i:], w) {
		p.i += len(w)
		return true
	}
	return false
}

func (p *checkpointJSONParser) value() (any, error) {
	if p.i >= len(p.s) {
		return nil, p.fail("Expecting value")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		p.i++
		return p.str()
	case p.word("true"):
		return true, nil
	case p.word("false"):
		return false, nil
	case p.word("null"):
		return nil, nil
	case p.word("NaN"):
		return math.NaN(), nil
	case p.word("Infinity"):
		return math.Inf(1), nil
	case p.word("-Infinity"):
		return math.Inf(-1), nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, p.fail("Expecting value")
}

// Number token kept as text in the json.Number form
func (p *checkpointJSONParser) number() (any, error) {
	start := p.i
	digits := func() int {
		n := 0
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
			n++
		}
		return n
	}
	if p.s[p.i] == '-' {
		p.i++
	}
	if p.i < len(p.s) && p.s[p.i] == '0' {
		p.i++
	} else if digits() == 0 {
		p.i = start
		return nil, p.fail("Expecting value")
	}
	if p.i < len(p.s) && p.s[p.i] == '.' {
		save := p.i
		p.i++
		if digits() == 0 {
			p.i = save
		}
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		save := p.i
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		if digits() == 0 {
			p.i = save
		}
	}
	return json.Number(p.s[start:p.i]), nil
}

// Four hex digits of a unicode escape
func (p *checkpointJSONParser) hex4() (rune, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	n, err := strconv.ParseUint(p.s[p.i:p.i+4], 16, 32)
	if err != nil {
		return 0, false
	}
	p.i += 4
	return rune(n), true
}

// String body after the opening quote, lone low surrogates in the byte range written back as raw bytes
func (p *checkpointJSONParser) str() (string, error) {
	var b strings.Builder
	for {
		if p.i >= len(p.s) {
			return "", p.fail("Unterminated string")
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), nil
		case c < 0x20:
			return "", p.fail("Invalid control character")
		case c != '\\':
			b.WriteByte(c)
			p.i++
			continue
		}
		p.i++
		if p.i >= len(p.s) {
			return "", p.fail("Unterminated string")
		}
		esc := p.s[p.i]
		p.i++
		if simple, ok := map[byte]string{'"': `"`, '\\': `\`, '/': "/", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t"}[esc]; ok {
			b.WriteString(simple)
			continue
		}
		if esc != 'u' {
			return "", p.fail(`Invalid \escape`)
		}
		r, ok := p.hex4()
		if !ok {
			return "", p.fail(`Invalid \uXXXX escape`)
		}
		if r >= 0xD800 && r < 0xDC00 && strings.HasPrefix(p.s[p.i:], `\u`) {
			save := p.i
			p.i += 2
			if r2, ok := p.hex4(); ok && r2 >= 0xDC00 && r2 <= 0xDFFF {
				b.WriteRune(utf16.DecodeRune(r, r2))
				continue
			}
			p.i = save
		}
		if r >= 0xDC80 && r <= 0xDCFF {
			b.WriteByte(byte(r - 0xDC00))
			continue
		}
		if r >= 0xD800 && r <= 0xDFFF {
			b.WriteRune(utf8.RuneError)
			continue
		}
		b.WriteRune(r)
	}
}

func (p *checkpointJSONParser) array() (any, error) {
	p.i++
	out := []any{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return out, nil
	}
	for {
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return out, nil
		}
		if p.i >= len(p.s) || p.s[p.i] != ',' {
			return nil, p.fail("Expecting ',' delimiter")
		}
		p.i++
	}
}

func (p *checkpointJSONParser) object() (any, error) {
	p.i++
	out := map[string]any{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return out, nil
	}
	for {
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, p.fail("Expecting property name enclosed in double quotes")
		}
		p.i++
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, p.fail("Expecting ':' delimiter")
		}
		p.i++
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out[key] = v
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return out, nil
		}
		if p.i >= len(p.s) || p.s[p.i] != ',' {
			return nil, p.fail("Expecting ',' delimiter")
		}
		p.i++
	}
}
