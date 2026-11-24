package jsonutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Nesting past which containers on the current path are remembered so a cycle is reported instead of recursing forever
const cycleDepth = 1000

// Most Convert results in a row that may themselves need converting
const maxConverts = 100

// Largest buffer kept for reuse between calls
const maxPooledBuffer = 64 << 10

type encoder struct {
	buf      []byte
	pretty   bool
	spaced   bool
	indent   string
	sortKeys bool
	ascii    bool
	convert  func(any) (any, error)
	depth    int
	ptrs     int
	converts int
	seen     map[visit]struct{}
}

// Identity of a container on the current path: a map, a slice with its length, or a pointer with its type
type visit struct {
	ptr uintptr
	n   int
	typ reflect.Type
}

// Setting for Encode and Append
type Option func(*encoder)

// Each array element and object member on its own line, indented n spaces a level and a space after each colon; zero puts them on their own lines without indentation
func Indent(n int) Option {
	unit := strings.Repeat(" ", max(n, 0))
	return func(e *encoder) { e.pretty, e.indent = true, unit }
}

// A space after each comma between items and after each colon on a single line, the layout some signature schemes recompute; Indent takes precedence
func Spaced() Option { return func(e *encoder) { e.spaced = true } }

// Keys of every *Object written in sorted order instead of insertion order
func SortKeys() Option { return func(e *encoder) { e.sortKeys = true } }

// Every character outside printable ASCII that is not escaped anyway (DEL and everything past it) written as a \u escape, astral ones as a surrogate pair
func ASCII() Option { return func(e *encoder) { e.ascii = true } }

// Hook for values with no JSON form (functions, channels, complex numbers, maps with unusable keys): fn's result is encoded in their place
func Convert(fn func(any) (any, error)) Option { return func(e *encoder) { e.convert = fn } }

var encoderPool = sync.Pool{New: func() any { return new(encoder) }}

func newEncoder(opts []Option) *encoder {
	e := encoderPool.Get().(*encoder)
	for _, opt := range opts {
		opt(e)
	}
	return e
}

func (e *encoder) release() {
	buf := e.buf[:0]
	if cap(buf) > maxPooledBuffer {
		buf = nil
	}
	clear(e.seen)
	*e = encoder{buf: buf, seen: e.seen}
	encoderPool.Put(e)
}

// JSON text of v: compact, UTF-8, no HTML escaping, *Object keys in insertion order, map keys sorted, nil slices and maps as [] and {}, every float with a fraction or exponent (1.0, never 1) so it decodes back as a float, NaN and infinities as null, structs by their json tags, json.Marshaler and encoding.TextMarshaler honored, errors as their text
func Encode(v any, opts ...Option) (string, error) {
	e := newEncoder(opts)
	defer e.release()
	if err := e.value(v); err != nil {
		return "", err
	}
	return string(e.buf), nil
}

// dst with the JSON text of v appended as Encode writes it
func Append(dst []byte, v any, opts ...Option) ([]byte, error) {
	e := newEncoder(opts)
	if cap(dst) == 0 {
		err := e.value(v)
		var out []byte
		if err == nil {
			out = bytes.Clone(e.buf)
		}
		e.release()
		if err != nil {
			return dst, err
		}
		return out, nil
	}
	own := e.buf
	e.buf = dst
	err := e.value(v)
	out := e.buf
	e.buf = own
	e.release()
	if err != nil {
		return dst, err
	}
	return out, nil
}

func (e *encoder) null() { e.buf = append(e.buf, "null"...) }

// Writes one value, the common dynamic shapes without reflection
func (e *encoder) value(v any) error {
	switch x := v.(type) {
	case nil:
		e.null()
	case string:
		e.buf = appendString(e.buf, x, e.ascii)
	case bool:
		e.buf = strconv.AppendBool(e.buf, x)
	case int:
		e.buf = strconv.AppendInt(e.buf, int64(x), 10)
	case int64:
		e.buf = strconv.AppendInt(e.buf, x, 10)
	case int32:
		e.buf = strconv.AppendInt(e.buf, int64(x), 10)
	case int16:
		e.buf = strconv.AppendInt(e.buf, int64(x), 10)
	case int8:
		e.buf = strconv.AppendInt(e.buf, int64(x), 10)
	case uint:
		e.buf = strconv.AppendUint(e.buf, uint64(x), 10)
	case uint64:
		e.buf = strconv.AppendUint(e.buf, x, 10)
	case uint32:
		e.buf = strconv.AppendUint(e.buf, uint64(x), 10)
	case uint16:
		e.buf = strconv.AppendUint(e.buf, uint64(x), 10)
	case uint8:
		e.buf = strconv.AppendUint(e.buf, uint64(x), 10)
	case float64:
		e.buf = appendFloat(e.buf, x, 64)
	case float32:
		e.buf = appendFloat(e.buf, float64(x), 32)
	case json.Number:
		return e.number(string(x))
	case *Object:
		if x == nil {
			e.null()
			return nil
		}
		return e.object(x)
	case []any:
		return e.array(x)
	case map[string]any:
		return e.stringMap(x)
	case []string:
		e.stringSlice(x)
	case json.RawMessage:
		if x == nil {
			e.null()
			return nil
		}
		return e.raw(x, reflect.TypeFor[json.RawMessage]())
	default:
		return e.reflectValue(reflect.ValueOf(v))
	}
	return nil
}

// Float as a JSON number that still reads as a float: shortest digits in the encoding/json layout, an integral value with a ".0" fraction (1.0, -0.0, 100.0), null for NaN and the infinities
func appendFloat(b []byte, f float64, size int) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(b, "null"...)
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 {
		if size == 64 && (abs < 1e-6 || abs >= 1e21) || size == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
			format = 'e'
		}
	}
	b = strconv.AppendFloat(b, f, format, -1, size)
	if format == 'e' {
		if n := len(b); n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
		return b
	}
	if f == math.Trunc(f) {
		b = append(b, '.', '0')
	}
	return b
}

func (e *encoder) number(s string) error {
	if s == "" {
		s = "0"
	}
	if !validNumber(s) {
		return fmt.Errorf("json: invalid number literal %q", s)
	}
	e.buf = append(e.buf, s...)
	return nil
}

// Whether s is a JSON number literal
func validNumber(s string) bool {
	d := decoder{data: view(s)}
	if s == "" || (s[0] != '-' && !isDigit(s[0])) {
		return false
	}
	_, err := d.number()
	return err == nil && d.pos == len(s)
}

func (e *encoder) newline() {
	if e.pretty {
		e.buf = append(e.buf, '\n')
		for range e.depth {
			e.buf = append(e.buf, e.indent...)
		}
	}
}

// Writes the opening bracket of the container v
func (e *encoder) open(c byte, v any) (visit, error) {
	if e.depth+e.ptrs >= cycleDepth {
		return e.openValue(c, reflect.ValueOf(v))
	}
	e.depth++
	e.buf = append(e.buf, c)
	return visit{}, nil
}

// Writes the opening bracket of the container v, past cycleDepth first checking it is not already on the path
func (e *encoder) openValue(c byte, v reflect.Value) (visit, error) {
	e.depth++
	var id visit
	if e.depth+e.ptrs > cycleDepth {
		switch v.Kind() {
		case reflect.Pointer, reflect.Map:
			id.ptr = uintptr(v.UnsafePointer())
		case reflect.Slice:
			id = visit{ptr: uintptr(v.UnsafePointer()), n: v.Len()}
		}
		if id.ptr != 0 {
			if _, ok := e.seen[id]; ok {
				return id, &json.UnsupportedValueError{Value: v, Str: "encountered a cycle via " + v.Type().String()}
			}
			if e.seen == nil {
				e.seen = map[visit]struct{}{}
			}
			e.seen[id] = struct{}{}
		}
	}
	e.buf = append(e.buf, c)
	return id, nil
}

// Writes the closing bracket on its own line when indenting and forgets the container's identity
func (e *encoder) close(c byte, id visit) {
	if id.ptr != 0 {
		delete(e.seen, id)
	}
	e.depth--
	e.newline()
	e.buf = append(e.buf, c)
}

// Writes the separator before the item at position i
func (e *encoder) item(i int) {
	if i > 0 {
		e.buf = append(e.buf, ',')
		if e.spaced && !e.pretty {
			e.buf = append(e.buf, ' ')
		}
	}
	e.newline()
}

func (e *encoder) key(k string) {
	e.buf = appendString(e.buf, k, e.ascii)
	e.colon()
}

func (e *encoder) colon() {
	if e.pretty || e.spaced {
		e.buf = append(e.buf, ':', ' ')
	} else {
		e.buf = append(e.buf, ':')
	}
}

func (e *encoder) object(o *Object) error {
	if len(o.keys) == 0 {
		e.buf = append(e.buf, "{}"...)
		return nil
	}
	id, err := e.open('{', o)
	if err != nil {
		return err
	}
	if e.sortKeys && !slices.IsSorted(o.keys) {
		order := make([]int, len(o.keys))
		for i := range order {
			order[i] = i
		}
		slices.SortFunc(order, func(a, b int) int { return strings.Compare(o.keys[a], o.keys[b]) })
		for n, i := range order {
			e.item(n)
			e.key(o.keys[i])
			if err := e.value(o.vals[i]); err != nil {
				return err
			}
		}
	} else {
		for i, k := range o.keys {
			e.item(i)
			e.key(k)
			if err := e.value(o.vals[i]); err != nil {
				return err
			}
		}
	}
	e.close('}', id)
	return nil
}

func (e *encoder) array(a []any) error {
	if len(a) == 0 {
		e.buf = append(e.buf, "[]"...)
		return nil
	}
	var id visit
	if e.depth+e.ptrs >= cycleDepth {
		var err error
		if id, err = e.openValue('[', reflect.ValueOf(a)); err != nil {
			return err
		}
	} else {
		e.depth++
		e.buf = append(e.buf, '[')
	}
	for i, v := range a {
		e.item(i)
		if err := e.value(v); err != nil {
			return err
		}
	}
	e.close(']', id)
	return nil
}

func (e *encoder) stringSlice(a []string) {
	if len(a) == 0 {
		e.buf = append(e.buf, "[]"...)
		return
	}
	e.depth++
	e.buf = append(e.buf, '[')
	for i, s := range a {
		e.item(i)
		e.buf = appendString(e.buf, s, e.ascii)
	}
	e.close(']', visit{})
}

func (e *encoder) stringMap(m map[string]any) error {
	if len(m) == 0 {
		e.buf = append(e.buf, "{}"...)
		return nil
	}
	id, err := e.open('{', m)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for i, k := range keys {
		e.item(i)
		e.key(k)
		if err := e.value(m[k]); err != nil {
			return err
		}
	}
	e.close('}', id)
	return nil
}

// Writes JSON text produced by a marshaler, checked, made valid UTF-8 and laid out to match the rest of the output
func (e *encoder) raw(b []byte, t reflect.Type) error {
	start := len(e.buf)
	buf := bytes.NewBuffer(e.buf)
	var err error
	if e.pretty {
		err = json.Indent(buf, bytes.TrimRight(b, " \t\r\n"), strings.Repeat(e.indent, e.depth), e.indent)
	} else {
		err = json.Compact(buf, b)
	}
	if err != nil {
		e.buf = e.buf[:start]
		return &json.MarshalerError{Type: t, Err: err}
	}
	e.buf = buf.Bytes()
	switch {
	case e.ascii:
		e.buf = escapeNonASCII(e.buf, start)
	case !utf8.Valid(e.buf[start:]):
		e.buf = append(e.buf[:start], validUTF8(e.buf[start:])...)
	}
	if e.spaced && !e.pretty {
		e.buf = spaceSeparators(e.buf, start)
	}
	return nil
}

// Hands a value with no JSON form to the Convert hook and writes what it gives back
func (e *encoder) converted(v reflect.Value) error {
	if e.convert == nil || !v.CanInterface() {
		return &json.UnsupportedTypeError{Type: v.Type()}
	}
	if e.converts++; e.converts > maxConverts {
		return fmt.Errorf("json: Convert keeps returning values with no JSON form, last %s", v.Type())
	}
	out, err := e.convert(v.Interface())
	if err != nil {
		return err
	}
	err = e.value(out)
	e.converts--
	return err
}

const hexDigits = "0123456789abcdef"

// Eight bytes of s from i as a little-endian word
func load64(s string, i int) uint64 {
	_ = s[i+7]
	return uint64(s[i]) | uint64(s[i+1])<<8 | uint64(s[i+2])<<16 | uint64(s[i+3])<<24 |
		uint64(s[i+4])<<32 | uint64(s[i+5])<<40 | uint64(s[i+6])<<48 | uint64(s[i+7])<<56
}

// Bit 7 set in each byte lane of x that needs escaping or is not ASCII, DEL included when ascii is set, exact for the lowest such lane
func escapeLanes(x uint64, ascii bool) uint64 {
	const ones, highs = 0x0101010101010101, 0x8080808080808080
	q, b := x^(ones*'"'), x^(ones*'\\')
	m := x | (x-ones*0x20)&^x | (q-ones)&^q | (b-ones)&^b
	if ascii {
		del := x ^ (ones * 0x7f)
		m |= (del - ones) &^ del
	}
	return m & highs
}

// dst with s appended as a quoted JSON string, invalid UTF-8 written as U+FFFD and non-ASCII escaped when ascii is set
func appendString(dst []byte, s string, ascii bool) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		for i+8 <= len(s) {
			if m := escapeLanes(load64(s, i), ascii); m != 0 {
				i += bits.TrailingZeros64(m) >> 3
				break
			}
			i += 8
		}
		if i >= len(s) {
			break
		}
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' && (c != 0x7f || !ascii) {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			if ascii {
				dst = append(dst, '\\', 'u', 'f', 'f', 'f', 'd')
			} else {
				dst = utf8.AppendRune(dst, utf8.RuneError)
			}
			i++
			start = i
			continue
		}
		if ascii {
			dst = append(dst, s[start:i]...)
			dst = appendRuneEscape(dst, r)
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// dst with r written as one \u escape, or a surrogate pair of them beyond the basic plane
func appendRuneEscape(dst []byte, r rune) []byte {
	hex4 := func(dst []byte, u rune) []byte {
		return append(dst, '\\', 'u', hexDigits[u>>12&0xf], hexDigits[u>>8&0xf], hexDigits[u>>4&0xf], hexDigits[u&0xf])
	}
	if r > 0xffff {
		r -= 0x10000
		dst = hex4(dst, 0xd800+r>>10)
		return hex4(dst, 0xdc00+r&0x3ff)
	}
	return hex4(dst, r)
}

// b with DEL and every non-ASCII character from start on, which in JSON text can only sit inside strings, rewritten as \u escapes
func escapeNonASCII(b []byte, start int) []byte {
	i := start
	for i < len(b) && b[i] < 0x7f {
		i++
	}
	if i == len(b) {
		return b
	}
	tail := slices.Clone(b[i:])
	b = b[:i]
	for j := 0; j < len(tail); {
		if tail[j] < 0x7f {
			b = append(b, tail[j])
			j++
			continue
		}
		r, size := utf8.DecodeRune(tail[j:])
		b = appendRuneEscape(b, r)
		j += size
	}
	return b
}

// b with a space added after each comma and colon from start on that sits outside a string
func spaceSeparators(b []byte, start int) []byte {
	n := 0
	inString := false
	for i := start; i < len(b); i++ {
		switch c := b[i]; {
		case inString && c == '\\':
			i++
		case c == '"':
			inString = !inString
		case !inString && (c == ',' || c == ':'):
			n++
		}
	}
	if n == 0 {
		return b
	}
	tail := slices.Clone(b[start:])
	b = b[:start]
	inString = false
	for i := 0; i < len(tail); i++ {
		c := tail[i]
		b = append(b, c)
		switch {
		case inString && c == '\\' && i+1 < len(tail):
			i++
			b = append(b, tail[i])
		case c == '"':
			inString = !inString
		case !inString && (c == ',' || c == ':'):
			b = append(b, ' ')
		}
	}
	return b
}
