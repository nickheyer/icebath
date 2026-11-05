package pm

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"
)

// JSON object that keeps first insertion order like a Python dict
type pyJSONObject struct {
	keys []string
	vals map[string]any
}

func (o *pyJSONObject) get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

func (o *pyJSONObject) set(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// JSON integer kept as its canonical decimal text since Python ints are unbounded
type pyJSONInt string

// JSON decode error formatted with line, column and character offset like json.JSONDecodeError
type pyJSONError struct {
	msg                string
	pos, lineno, colno int
}

func (e *pyJSONError) Error() string {
	return fmt.Sprintf("%s: line %d column %d (char %d)", e.msg, e.lineno, e.colno, e.pos)
}

type pyJSONParser struct{ s []rune }

// Signals that no value starts at the offset, the StopIteration of the Python scanner
type pyJSONStop struct{ pos int }

func (e *pyJSONStop) Error() string { return "Expecting value" }

func (p *pyJSONParser) fail(msg string, pos int) error {
	lineno, last := 1, -1
	for i := 0; i < pos && i < len(p.s); i++ {
		if p.s[i] == '\n' {
			lineno++
			last = i
		}
	}
	return &pyJSONError{msg: msg, pos: pos, lineno: lineno, colno: pos - last}
}

func (p *pyJSONParser) ws(i int) int {
	for i < len(p.s) && (p.s[i] == ' ' || p.s[i] == '\t' || p.s[i] == '\n' || p.s[i] == '\r') {
		i++
	}
	return i
}

// Decodes JSON text with the json.loads grammar, NaN and Infinity included
func pyJSONLoads(text string) (any, error) {
	p := &pyJSONParser{s: []rune(text)}
	idx := p.ws(0)
	v, end, err := p.value(idx)
	if stop, ok := err.(*pyJSONStop); ok {
		return nil, p.fail("Expecting value", stop.pos)
	}
	if err != nil {
		return nil, err
	}
	if end = p.ws(end); end != len(p.s) {
		return nil, p.fail("Extra data", end)
	}
	return v, nil
}

func (p *pyJSONParser) hasWord(i int, w string) bool {
	rs := []rune(w)
	if i+len(rs) > len(p.s) {
		return false
	}
	for k, r := range rs {
		if p.s[i+k] != r {
			return false
		}
	}
	return true
}

func (p *pyJSONParser) value(i int) (any, int, error) {
	if i >= len(p.s) {
		return nil, i, &pyJSONStop{i}
	}
	switch c := p.s[i]; {
	case c == '"':
		return p.str(i + 1)
	case c == '{':
		return p.object(i + 1)
	case c == '[':
		return p.array(i + 1)
	case c == 'n' && p.hasWord(i, "null"):
		return nil, i + 4, nil
	case c == 't' && p.hasWord(i, "true"):
		return true, i + 4, nil
	case c == 'f' && p.hasWord(i, "false"):
		return false, i + 5, nil
	case c == 'N' && p.hasWord(i, "NaN"):
		return math.NaN(), i + 3, nil
	case c == 'I' && p.hasWord(i, "Infinity"):
		return math.Inf(1), i + 8, nil
	case c == '-' && p.hasWord(i, "-Infinity"):
		return math.Inf(-1), i + 9, nil
	}
	return p.number(i)
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

func (p *pyJSONParser) number(i int) (any, int, error) {
	j := i
	if j < len(p.s) && p.s[j] == '-' {
		j++
	}
	switch {
	case j < len(p.s) && p.s[j] == '0':
		j++
	case j < len(p.s) && p.s[j] >= '1' && p.s[j] <= '9':
		for j < len(p.s) && isASCIIDigit(p.s[j]) {
			j++
		}
	default:
		return nil, i, &pyJSONStop{i}
	}
	isFloat := false
	if j+1 < len(p.s) && p.s[j] == '.' && isASCIIDigit(p.s[j+1]) {
		isFloat = true
		j += 2
		for j < len(p.s) && isASCIIDigit(p.s[j]) {
			j++
		}
	}
	if j < len(p.s) && (p.s[j] == 'e' || p.s[j] == 'E') {
		k := j + 1
		if k < len(p.s) && (p.s[k] == '+' || p.s[k] == '-') {
			k++
		}
		if k < len(p.s) && isASCIIDigit(p.s[k]) {
			for k < len(p.s) && isASCIIDigit(p.s[k]) {
				k++
			}
			isFloat, j = true, k
		}
	}
	text := string(p.s[i:j])
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !strings.Contains(err.Error(), "value out of range") {
			return nil, i, err
		}
		return f, j, nil
	}
	n, _ := new(big.Int).SetString(text, 10)
	return pyJSONInt(n.String()), j, nil
}

func hexRune(rs []rune) (rune, bool) {
	var v rune
	for _, r := range rs {
		v <<= 4
		switch {
		case r >= '0' && r <= '9':
			v |= r - '0'
		case r >= 'a' && r <= 'f':
			v |= r - 'a' + 10
		case r >= 'A' && r <= 'F':
			v |= r - 'A' + 10
		default:
			return 0, false
		}
	}
	return v, true
}

// Scans a string body starting after its opening quote
func (p *pyJSONParser) str(end int) (any, int, error) {
	begin := end - 1
	var b strings.Builder
	for {
		next := end
		for next < len(p.s) && p.s[next] != '"' && p.s[next] != '\\' && p.s[next] > 0x1f {
			next++
		}
		if next >= len(p.s) {
			return nil, end, p.fail("Unterminated string starting at", begin)
		}
		b.WriteString(wtf8FromRunes(p.s[end:next]))
		c := p.s[next]
		if c == '"' {
			return b.String(), next + 1, nil
		}
		if c != '\\' {
			return nil, end, p.fail("Invalid control character at", next)
		}
		next++
		if next >= len(p.s) {
			return nil, end, p.fail("Unterminated string starting at", begin)
		}
		c = p.s[next]
		if c != 'u' {
			end = next + 1
			esc := map[rune]string{'"': `"`, '\\': `\`, '/': "/", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t"}[c]
			if esc == "" {
				return nil, end, p.fail(`Invalid \escape`, end-2)
			}
			b.WriteString(esc)
			continue
		}
		next++
		end = next + 4
		if end >= len(p.s) {
			return nil, end, p.fail(`Invalid \uXXXX escape`, next-1)
		}
		r, ok := hexRune(p.s[next:end])
		if !ok {
			return nil, end, p.fail(`Invalid \uXXXX escape`, end-5)
		}
		if utf16.IsSurrogate(r) && r < 0xDC00 && end+6 < len(p.s) && p.s[end] == '\\' && p.s[end+1] == 'u' {
			r2, ok := hexRune(p.s[end+2 : end+6])
			if !ok {
				return nil, end, p.fail(`Invalid \uXXXX escape`, end+1)
			}
			if r2 >= 0xDC00 && r2 <= 0xDFFF {
				r = utf16.DecodeRune(r, r2)
				end += 6
			}
		}
		wtf8WriteRune(&b, r)
	}
}

func (p *pyJSONParser) object(i int) (any, int, error) {
	obj := &pyJSONObject{vals: map[string]any{}}
	i = p.ws(i)
	if i < len(p.s) && p.s[i] == '}' {
		return obj, i + 1, nil
	}
	for {
		if i >= len(p.s) || p.s[i] != '"' {
			return nil, i, p.fail("Expecting property name enclosed in double quotes", i)
		}
		key, next, err := p.str(i + 1)
		if err != nil {
			return nil, next, err
		}
		i = p.ws(next)
		if i >= len(p.s) || p.s[i] != ':' {
			return nil, i, p.fail("Expecting ':' delimiter", i)
		}
		i = p.ws(i + 1)
		val, next, err := p.value(i)
		if err != nil {
			return nil, next, err
		}
		obj.set(key.(string), val)
		i = p.ws(next)
		if i < len(p.s) && p.s[i] == '}' {
			return obj, i + 1, nil
		}
		if i >= len(p.s) || p.s[i] != ',' {
			return nil, i, p.fail("Expecting ',' delimiter", i)
		}
		comma := i
		i = p.ws(i + 1)
		if i < len(p.s) && p.s[i] == '}' {
			return nil, i, p.fail("Illegal trailing comma before end of object", comma)
		}
	}
}

func (p *pyJSONParser) array(i int) (any, int, error) {
	out := []any{}
	i = p.ws(i)
	if i < len(p.s) && p.s[i] == ']' {
		return out, i + 1, nil
	}
	for {
		val, next, err := p.value(i)
		if err != nil {
			return nil, next, err
		}
		out = append(out, val)
		i = p.ws(next)
		if i < len(p.s) && p.s[i] == ']' {
			return out, i + 1, nil
		}
		if i >= len(p.s) || p.s[i] != ',' {
			return nil, i, p.fail("Expecting ',' delimiter", i)
		}
		comma := i
		i = p.ws(i + 1)
		if i < len(p.s) && p.s[i] == ']' {
			return nil, i, p.fail("Illegal trailing comma before end of array", comma)
		}
	}
}

// Code points as a string, each lone surrogate written in its three byte generalized UTF-8 form
func wtf8FromRunes(rs []rune) string {
	var b strings.Builder
	for _, r := range rs {
		wtf8WriteRune(&b, r)
	}
	return b.String()
}

// Writes a code point, a surrogate taking the three byte form UTF-8 refuses
func wtf8WriteRune(b *strings.Builder, r rune) {
	if r >= 0xD800 && r <= 0xDFFF {
		b.WriteByte(byte(0xE0 | r>>12))
		b.WriteByte(byte(0x80 | (r>>6)&0x3F))
		b.WriteByte(byte(0x80 | r&0x3F))
		return
	}
	b.WriteRune(r)
}
