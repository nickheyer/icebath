package jsonutil

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"reflect"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// Deepest nesting of arrays and objects the decoder accepts
const maxDepth = 10000

const msgEOF = "unexpected end of input"

type member struct {
	key string
	val any
}

type decoder struct {
	data    []byte
	pos     int
	depth   int
	plain   bool
	control bool
	unique  bool
	hitEOF  bool
	members []member
	values  []any
	scratch []byte
}

// Setting for DecodeWith
type DecodeOption func(*decoder)

// Raw control characters inside strings taken as they are instead of refused
func ControlChars() DecodeOption { return func(d *decoder) { d.control = true } }

// A key repeated inside one object refused instead of its last value kept at its first position
func UniqueKeys() DecodeOption { return func(d *decoder) { d.unique = true } }

// Value of the JSON text in data: objects as *Object in document order, arrays as []any, integers that fit as int64, other numbers as float64, NaN and Infinity accepted, strings made valid UTF-8, a byte order mark and UTF-16 or UTF-32 input understood, anything after the value refused
func Decode[T ~string | ~[]byte](data T) (any, error) {
	var d decoder
	return d.document(view(data))
}

// Object of the JSON text in data as Decode reads it, ErrNotObject wrapped when the text holds another kind of value
func DecodeObject[T ~string | ~[]byte](data T) (*Object, error) {
	v, err := Decode(data)
	if err != nil {
		return nil, err
	}
	o, ok := v.(*Object)
	if !ok {
		return nil, fmt.Errorf("%w: found %s", ErrNotObject, kindName(v))
	}
	return o, nil
}

// Value of the JSON text in data in the shapes encoding/json gives an any, objects as map[string]any and every number as float64, otherwise read as Decode reads it
func DecodePlain[T ~string | ~[]byte](data T) (any, error) {
	d := decoder{plain: true}
	return d.document(view(data))
}

// Value of the JSON text in data as Decode reads it under the given options
func DecodeWith[T ~string | ~[]byte](data T, opts ...DecodeOption) (any, error) {
	var d decoder
	for _, opt := range opts {
		opt(&d)
	}
	return d.document(view(data))
}

// Bytes of data without a copy, the decoder only reading them
func view[T ~string | ~[]byte](data T) []byte {
	if reflect.TypeFor[T]().Kind() == reflect.String {
		s := string(data)
		return unsafe.Slice(unsafe.StringData(s), len(s))
	}
	return []byte(data)
}

func (d *decoder) document(data []byte) (any, error) {
	d.data, _ = toUTF8(data)
	d.skipSpace()
	v, err := d.value()
	if err != nil {
		return nil, err
	}
	d.skipSpace()
	if d.pos < len(d.data) {
		return nil, d.unexpected(d.pos, "after top-level value")
	}
	return v, nil
}

func (d *decoder) fail(offset int, msg string) error { return syntaxError(d.data, offset, msg) }

func (d *decoder) eof() error {
	d.hitEOF = true
	return syntaxError(d.data, len(d.data), msgEOF)
}

// Error for the character at offset, the end of input reported as such
func (d *decoder) unexpected(offset int, context string) error {
	if offset >= len(d.data) {
		return d.eof()
	}
	return d.fail(offset, "invalid character "+quoteAt(d.data, offset)+" "+context)
}

func (d *decoder) skipSpace() {
	for d.pos < len(d.data) {
		switch d.data[d.pos] {
		case ' ', '\t', '\n', '\r':
			d.pos++
		default:
			return
		}
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (d *decoder) value() (any, error) {
	if d.pos >= len(d.data) {
		return nil, d.eof()
	}
	c := d.data[d.pos]
	switch c {
	case '{':
		return d.object()
	case '[':
		return d.array()
	case '"':
		s, err := d.str()
		if err != nil {
			return nil, err
		}
		return s, nil
	case 't':
		return true, d.literal("true")
	case 'f':
		return false, d.literal("false")
	case 'n':
		return nil, d.literal("null")
	case 'N':
		return math.NaN(), d.literal("NaN")
	case 'I':
		return math.Inf(1), d.literal("Infinity")
	case '-':
		if d.pos+1 < len(d.data) && d.data[d.pos+1] == 'I' {
			return math.Inf(-1), d.literal("-Infinity")
		}
		return d.number()
	}
	if isDigit(c) {
		return d.number()
	}
	return nil, d.unexpected(d.pos, "looking for beginning of value")
}

func (d *decoder) literal(word string) error {
	for i := 0; i < len(word); i++ {
		p := d.pos + i
		if p >= len(d.data) {
			return d.eof()
		}
		if d.data[p] != word[i] {
			return d.unexpected(p, "in literal "+word+" (expecting "+quoteAt([]byte(word), i)+")")
		}
	}
	d.pos += len(word)
	return nil
}

func (d *decoder) number() (any, error) {
	data := d.data
	start := d.pos
	i := start
	if data[i] == '-' {
		i++
	}
	switch {
	case i >= len(data):
		return nil, d.eof()
	case data[i] == '0':
		i++
	case data[i] >= '1' && data[i] <= '9':
		for i++; i < len(data) && isDigit(data[i]); i++ {
		}
	default:
		return nil, d.unexpected(i, "in numeric literal")
	}
	integer := true
	if i < len(data) && data[i] == '.' {
		integer = false
		if i++; i >= len(data) || !isDigit(data[i]) {
			return nil, d.unexpected(i, "after decimal point in numeric literal")
		}
		for i < len(data) && isDigit(data[i]) {
			i++
		}
	}
	if i < len(data) && (data[i] == 'e' || data[i] == 'E') {
		integer = false
		if i++; i < len(data) && (data[i] == '+' || data[i] == '-') {
			i++
		}
		if i >= len(data) || !isDigit(data[i]) {
			return nil, d.unexpected(i, "in exponent of numeric literal")
		}
		for i < len(data) && isDigit(data[i]) {
			i++
		}
	}
	d.pos = i
	text := data[start:i]
	if integer && !d.plain {
		if n, ok := parseInt(text); ok {
			return n, nil
		}
	}
	f, _ := strconv.ParseFloat(unsafe.String(&text[0], len(text)), 64)
	return f, nil
}

// int64 of a JSON integer literal, false when it does not fit
func parseInt(b []byte) (int64, bool) {
	neg := b[0] == '-'
	if neg {
		b = b[1:]
	}
	if len(b) > 19 {
		return 0, false
	}
	var u uint64
	for _, c := range b {
		u = u*10 + uint64(c-'0')
	}
	if neg {
		if u > 1<<63 {
			return 0, false
		}
		return -int64(u), true
	}
	if u > math.MaxInt64 {
		return 0, false
	}
	return int64(u), true
}

// Bit 7 set in each byte lane of x holding a quote, a backslash or a control character, exact for the lowest such lane
func stopBytes(x uint64) uint64 {
	const ones, highs = 0x0101010101010101, 0x8080808080808080
	q, b := x^(ones*'"'), x^(ones*'\\')
	return ((x-ones*0x20)&^x | (q-ones)&^q | (b-ones)&^b) & highs
}

// Offset of the first quote, backslash or control character at or after i, len(data) when there is none
func skipPlain(data []byte, i int) int {
	for i+8 <= len(data) {
		if m := stopBytes(binary.LittleEndian.Uint64(data[i:])); m != 0 {
			return i + bits.TrailingZeros64(m)>>3
		}
		i += 8
	}
	for i < len(data) {
		if c := data[i]; c == '"' || c == '\\' || c < 0x20 {
			return i
		}
		i++
	}
	return i
}

// String literal starting at the quote at d.pos with its escapes resolved and invalid UTF-8 replaced by U+FFFD
func (d *decoder) str() (string, error) {
	start := d.pos + 1
	i := skipPlain(d.data, start)
	if i < len(d.data) && d.data[i] == '"' {
		d.pos = i + 1
		return string(validUTF8(d.data[start:i])), nil
	}
	return d.strEscaped(start, i)
}

func (d *decoder) strEscaped(start, i int) (string, error) {
	data := d.data
	buf := append(d.scratch[:0], data[start:i]...)
	for {
		j := skipPlain(data, i)
		buf = append(buf, data[i:j]...)
		d.scratch = buf[:0]
		if i = j; i >= len(data) {
			return "", d.eof()
		}
		c := data[i]
		if c == '"' {
			d.pos = i + 1
			return string(validUTF8(buf)), nil
		}
		if c != '\\' {
			if !d.control {
				return "", d.unexpected(i, "in string literal")
			}
			buf = append(buf, c)
			i++
			continue
		}
		if i+1 >= len(data) {
			return "", d.eof()
		}
		switch esc := data[i+1]; esc {
		case '"', '\\', '/':
			buf = append(buf, esc)
		case 'b':
			buf = append(buf, '\b')
		case 'f':
			buf = append(buf, '\f')
		case 'n':
			buf = append(buf, '\n')
		case 'r':
			buf = append(buf, '\r')
		case 't':
			buf = append(buf, '\t')
		case 'u':
			r, bad := hex4(data, i+2)
			if bad >= 0 {
				return "", d.unexpected(bad, `in \u hexadecimal character escape`)
			}
			i += 6
			if utf16.IsSurrogate(r) {
				lo, loBad := rune(-1), 0
				if r < 0xdc00 && i+1 < len(data) && data[i] == '\\' && data[i+1] == 'u' {
					lo, loBad = hex4(data, i+2)
				}
				if loBad < 0 && lo >= 0xdc00 && lo <= 0xdfff {
					r = utf16.DecodeRune(r, lo)
					i += 6
				} else {
					r = utf8.RuneError
				}
			}
			buf = utf8.AppendRune(buf, r)
			continue
		default:
			return "", d.unexpected(i+1, "in string escape code")
		}
		i += 2
	}
}

// Value of the four hex digits at i and -1, or the offset of the first character that is not one
func hex4(data []byte, i int) (rune, int) {
	var r rune
	for k := i; k < i+4; k++ {
		if k >= len(data) {
			return 0, len(data)
		}
		c := data[k]
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c -= 'a' - 10
		case c >= 'A' && c <= 'F':
			c -= 'A' - 10
		default:
			return 0, k
		}
		r = r<<4 | rune(c)
	}
	return r, -1
}

func (d *decoder) enter() error {
	if d.depth++; d.depth > maxDepth {
		return d.fail(d.pos, "exceeded max depth")
	}
	d.pos++
	return nil
}

func (d *decoder) object() (any, error) {
	if err := d.enter(); err != nil {
		return nil, err
	}
	start := len(d.members)
	var index map[string]int
	d.skipSpace()
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		d.depth--
		return d.finishObject(start, nil), nil
	}
	for {
		if d.pos >= len(d.data) || d.data[d.pos] != '"' {
			return nil, d.unexpected(d.pos, "looking for beginning of object key string")
		}
		keyAt := d.pos
		key, err := d.str()
		if err != nil {
			return nil, err
		}
		d.skipSpace()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return nil, d.unexpected(d.pos, "after object key")
		}
		d.pos++
		d.skipSpace()
		val, err := d.value()
		if err != nil {
			return nil, err
		}
		if index, err = d.addMember(start, index, key, val, keyAt); err != nil {
			return nil, err
		}
		d.skipSpace()
		if d.pos >= len(d.data) {
			return nil, d.eof()
		}
		switch d.data[d.pos] {
		case ',':
			d.pos++
			d.skipSpace()
		case '}':
			d.pos++
			d.depth--
			return d.finishObject(start, index), nil
		default:
			return nil, d.unexpected(d.pos, "after object key:value pair")
		}
	}
}

// Pushes one member of the object whose members start at start, a repeated key replacing the value at its first position
func (d *decoder) addMember(start int, index map[string]int, key string, val any, keyAt int) (map[string]int, error) {
	check := !d.plain || d.unique
	if check {
		at := -1
		if index != nil {
			if i, ok := index[key]; ok {
				at = i
			}
		} else {
			for i, m := range d.members[start:] {
				if m.key == key {
					at = i
					break
				}
			}
		}
		if at >= 0 {
			if d.unique {
				return index, d.fail(keyAt, "duplicate key "+strconv.Quote(key))
			}
			d.members[start+at].val = val
			return index, nil
		}
	}
	n := len(d.members) - start
	d.members = append(d.members, member{key, val})
	if check {
		switch {
		case index != nil:
			index[key] = n
		case n+1 > indexMin:
			index = make(map[string]int, 2*(n+1))
			for i, m := range d.members[start:] {
				index[m.key] = i
			}
		}
	}
	return index, nil
}

// Object or map of the members pushed since start, which are then popped
func (d *decoder) finishObject(start int, index map[string]int) any {
	ms := d.members[start:]
	var out any
	if d.plain {
		m := make(map[string]any, len(ms))
		for _, mb := range ms {
			m[mb.key] = mb.val
		}
		out = m
	} else {
		o := &Object{}
		if len(ms) > 0 {
			o.keys, o.vals = make([]string, len(ms)), make([]any, len(ms))
			for i, mb := range ms {
				o.keys[i], o.vals[i] = mb.key, mb.val
			}
			o.index = index
		}
		out = o
	}
	clear(ms)
	d.members = d.members[:start]
	return out
}

func (d *decoder) array() (any, error) {
	if err := d.enter(); err != nil {
		return nil, err
	}
	start := len(d.values)
	d.skipSpace()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		d.depth--
		return []any{}, nil
	}
	for {
		val, err := d.value()
		if err != nil {
			return nil, err
		}
		d.values = append(d.values, val)
		d.skipSpace()
		if d.pos >= len(d.data) {
			return nil, d.eof()
		}
		switch d.data[d.pos] {
		case ',':
			d.pos++
			d.skipSpace()
		case ']':
			d.pos++
			d.depth--
			return d.finishArray(start), nil
		default:
			return nil, d.unexpected(d.pos, "after array element")
		}
	}
}

// Array of the values pushed since start, which are then popped
func (d *decoder) finishArray(start int) []any {
	vs := d.values[start:]
	out := make([]any, len(vs))
	copy(out, vs)
	clear(vs)
	d.values = d.values[:start]
	return out
}
