package state

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// JSONDecodeError text with the line, column and character offset json.loads reports
type pyJSONDecodeError struct {
	Msg                string
	Pos, Lineno, Colno int
}

func (e *pyJSONDecodeError) Error() string {
	return fmt.Sprintf("%s: line %d column %d (char %d)", e.Msg, e.Lineno, e.Colno, e.Pos)
}

type pyJSONScanner struct{ s []rune }

// No value starts at the offset, the StopIteration of the Python scanner
type pyJSONStop struct{ pos int }

func (e *pyJSONStop) Error() string { return "Expecting value" }

func (p *pyJSONScanner) fail(msg string, pos int) error {
	lineno, last := 1, -1
	for i := 0; i < pos && i < len(p.s); i++ {
		if p.s[i] == '\n' {
			lineno++
			last = i
		}
	}
	return &pyJSONDecodeError{Msg: msg, Pos: pos, Lineno: lineno, Colno: pos - last}
}

func (p *pyJSONScanner) ws(i int) int {
	for i < len(p.s) && (p.s[i] == ' ' || p.s[i] == '\t' || p.s[i] == '\n' || p.s[i] == '\r') {
		i++
	}
	return i
}

// Decodes text with the json.loads grammar into pyObject, json.Number for ints, float64, string, []any, bool and nil
func pyLoads(text string) (any, error) {
	p := &pyJSONScanner{s: []rune(text)}
	if len(p.s) > 0 && p.s[0] == 0xfeff {
		return nil, p.fail("Unexpected UTF-8 BOM (decode using utf-8-sig)", 0)
	}
	v, end, err := p.value(p.ws(0))
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

func (p *pyJSONScanner) hasWord(i int, w string) bool {
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

func (p *pyJSONScanner) value(i int) (any, int, error) {
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

func asciiDigit(r rune) bool { return r >= '0' && r <= '9' }

func (p *pyJSONScanner) number(i int) (any, int, error) {
	j := i
	if j < len(p.s) && p.s[j] == '-' {
		j++
	}
	switch {
	case j < len(p.s) && p.s[j] == '0':
		j++
	case j < len(p.s) && p.s[j] >= '1' && p.s[j] <= '9':
		for j < len(p.s) && asciiDigit(p.s[j]) {
			j++
		}
	default:
		return nil, i, &pyJSONStop{i}
	}
	isFloat := false
	if j+1 < len(p.s) && p.s[j] == '.' && asciiDigit(p.s[j+1]) {
		isFloat = true
		j += 2
		for j < len(p.s) && asciiDigit(p.s[j]) {
			j++
		}
	}
	if j < len(p.s) && (p.s[j] == 'e' || p.s[j] == 'E') {
		k := j + 1
		if k < len(p.s) && (p.s[k] == '+' || p.s[k] == '-') {
			k++
		}
		if k < len(p.s) && asciiDigit(p.s[k]) {
			for k < len(p.s) && asciiDigit(p.s[k]) {
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
	if strings.HasPrefix(text, "-0") && len(text) == 2 {
		text = "0"
	}
	return json.Number(text), j, nil
}

func (p *pyJSONScanner) hex4(at int) (rune, bool) {
	if at+4 >= len(p.s) {
		return 0, false
	}
	var v rune
	for _, r := range p.s[at : at+4] {
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

// Scans a string body after its opening quote, lone surrogates kept in generalized UTF-8
func (p *pyJSONScanner) str(end int) (any, int, error) {
	begin := end - 1
	var b strings.Builder
	escapes := map[rune]string{'"': `"`, '\\': `\`, '/': "/", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t"}
	for {
		next := end
		for next < len(p.s) && p.s[next] != '"' && p.s[next] != '\\' && p.s[next] > 0x1f {
			next++
		}
		if next >= len(p.s) {
			return nil, end, p.fail("Unterminated string starting at", begin)
		}
		for _, r := range p.s[end:next] {
			wtf8Write(&b, r)
		}
		if p.s[next] == '"' {
			return b.String(), next + 1, nil
		}
		if p.s[next] != '\\' {
			return nil, end, p.fail("Invalid control character at", next)
		}
		next++
		if next >= len(p.s) {
			return nil, end, p.fail("Unterminated string starting at", begin)
		}
		if c := p.s[next]; c != 'u' {
			esc, ok := escapes[c]
			if !ok {
				return nil, end, p.fail(`Invalid \escape`, next-1)
			}
			b.WriteString(esc)
			end = next + 1
			continue
		}
		r, ok := p.hex4(next + 1)
		if !ok {
			return nil, end, p.fail(`Invalid \uXXXX escape`, next)
		}
		end = next + 5
		if r >= 0xd800 && r <= 0xdbff && end+6 < len(p.s) && p.s[end] == '\\' && p.s[end+1] == 'u' {
			r2, ok := p.hex4(end + 2)
			if !ok {
				return nil, end, p.fail(`Invalid \uXXXX escape`, end+1)
			}
			if r2 >= 0xdc00 && r2 <= 0xdfff {
				r = utf16.DecodeRune(r, r2)
				end += 6
			}
		}
		wtf8Write(&b, r)
	}
}

func (p *pyJSONScanner) object(i int) (any, int, error) {
	obj := pyObject{}
	index := map[string]int{}
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
		val, next, err := p.value(p.ws(i + 1))
		if stop, ok := err.(*pyJSONStop); ok {
			return nil, next, p.fail("Expecting value", stop.pos)
		}
		if err != nil {
			return nil, next, err
		}
		k := key.(string)
		if at, seen := index[k]; seen {
			obj[at].Value = val
		} else {
			index[k] = len(obj)
			obj = append(obj, pyKeyValue{k, val})
		}
		i = p.ws(next)
		if i < len(p.s) && p.s[i] == '}' {
			return obj, i + 1, nil
		}
		if i >= len(p.s) || p.s[i] != ',' {
			return nil, i, p.fail("Expecting ',' delimiter", i)
		}
		comma := i
		if i = p.ws(i + 1); i < len(p.s) && p.s[i] == '}' {
			return nil, i, p.fail("Illegal trailing comma before end of object", comma)
		}
	}
}

func (p *pyJSONScanner) array(i int) (any, int, error) {
	out := []any{}
	i = p.ws(i)
	if i < len(p.s) && p.s[i] == ']' {
		return out, i + 1, nil
	}
	for {
		val, next, err := p.value(i)
		if stop, ok := err.(*pyJSONStop); ok {
			return nil, next, p.fail("Expecting value", stop.pos)
		}
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
		if i = p.ws(i + 1); i < len(p.s) && p.s[i] == ']' {
			return nil, i, p.fail("Illegal trailing comma before end of array", comma)
		}
	}
}

// Writes a code point, a surrogate taking the three byte form UTF-8 refuses
func wtf8Write(b *strings.Builder, r rune) {
	if r >= 0xd800 && r <= 0xdfff {
		b.WriteByte(byte(0xe0 | r>>12))
		b.WriteByte(byte(0x80 | (r>>6)&0x3f))
		b.WriteByte(byte(0x80 | r&0x3f))
		return
	}
	b.WriteRune(r)
}

// Text of bytes under json.detect_encoding, decoded with surrogatepass, the UnicodeDecodeError on bad input
func pyJSONDecodeBytes(b []byte) (string, error) {
	enc := "utf-8"
	switch {
	case strings.HasPrefix(string(b), "\x00\x00\xfe\xff") || strings.HasPrefix(string(b), "\xff\xfe\x00\x00"):
		enc = "utf-32"
	case strings.HasPrefix(string(b), "\xfe\xff") || strings.HasPrefix(string(b), "\xff\xfe"):
		enc = "utf-16"
	case strings.HasPrefix(string(b), "\xef\xbb\xbf"):
		return pyDecodeUTF8Strict(b[3:])
	case len(b) >= 4 && b[0] == 0:
		enc = map[bool]string{true: "utf-16-be", false: "utf-32-be"}[b[1] != 0]
	case len(b) >= 4 && b[1] == 0:
		enc = map[bool]string{true: "utf-16-le", false: "utf-32-le"}[b[2] != 0 || b[3] != 0]
	case len(b) == 2 && b[0] == 0:
		enc = "utf-16-be"
	case len(b) == 2 && b[1] == 0:
		enc = "utf-16-le"
	}
	bigEndian := strings.HasSuffix(enc, "-be")
	switch {
	case enc == "utf-8":
		return pyDecodeUTF8Strict(b)
	case strings.HasPrefix(enc, "utf-16"):
		if enc == "utf-16" {
			bigEndian, b = b[0] == 0xfe, b[2:]
		}
		if len(b)%2 != 0 {
			return "", &pyConvError{"UnicodeDecodeError", "'" + enc + "' codec can't decode byte: truncated data"}
		}
		var sb strings.Builder
		for i := 0; i < len(b); i += 2 {
			u := rune(b[i+1])<<8 | rune(b[i])
			if bigEndian {
				u = rune(b[i])<<8 | rune(b[i+1])
			}
			if u >= 0xd800 && u <= 0xdbff && i+3 < len(b) {
				lo := rune(b[i+3])<<8 | rune(b[i+2])
				if bigEndian {
					lo = rune(b[i+2])<<8 | rune(b[i+3])
				}
				if lo >= 0xdc00 && lo <= 0xdfff {
					sb.WriteRune(utf16.DecodeRune(u, lo))
					i += 2
					continue
				}
			}
			wtf8Write(&sb, u)
		}
		return sb.String(), nil
	}
	if enc == "utf-32" {
		bigEndian, b = b[0] == 0, b[4:]
	}
	if len(b)%4 != 0 {
		return "", &pyConvError{"UnicodeDecodeError", "'" + enc + "' codec can't decode bytes: truncated data"}
	}
	var sb strings.Builder
	for i := 0; i < len(b); i += 4 {
		u := rune(b[i+3])<<24 | rune(b[i+2])<<16 | rune(b[i+1])<<8 | rune(b[i])
		if bigEndian {
			u = rune(b[i])<<24 | rune(b[i+1])<<16 | rune(b[i+2])<<8 | rune(b[i+3])
		}
		if u < 0 || u > 0x10ffff {
			return "", &pyConvError{"UnicodeDecodeError", "'" + enc + "' codec can't decode bytes: code point not in range(0x110000)"}
		}
		wtf8Write(&sb, u)
	}
	return sb.String(), nil
}

// UTF-8 bytes as text with surrogatepass, the UnicodeDecodeError at the first invalid byte
func pyDecodeUTF8Strict(b []byte) (string, error) {
	for i := 0; i < len(b); {
		if r, ok := wtf8Surrogate(string(b[i:])); ok && r >= 0xd800 {
			i += 3
			continue
		}
		if b[i] < 0x80 {
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		reason := "invalid continuation byte"
		if b[i] < 0xc2 || b[i] > 0xf4 {
			reason = "invalid start byte"
		} else if sub := pyDecodeUTF8Replace(b[i:]); sub == "\ufffd" {
			reason = "unexpected end of data"
		}
		return "", &pyConvError{"UnicodeDecodeError", fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", b[i], i, reason)}
	}
	return string(b), nil
}

// Insertion ordered JSON object other packages read and build for identity and decoded JSON values
type PyObject = pyObject

// One entry of a PyObject
type PyKeyValue = pyKeyValue
