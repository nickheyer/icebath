package pyrepr

import (
	"encoding/json"
	"fmt"
	"iter"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Renders as a Python tuple
type Tuple []any

// Renders as a Python dict in its own key order
type Mapping interface {
	PyItems() iter.Seq2[any, any]
}

// Supplies its own Python repr text
type Reprer interface{ PyRepr() string }

// Supplies its own Python str text
type Strer interface{ PyStr() string }

// Supplies its own Python type name
type TypeNamer interface{ PyTypeName() string }

// Supplies its own Python truth value
type Booler interface{ PyBool() bool }

// Python repr of a str with CPython quote choice, escapes and isprintable test on WTF-8 text
func StrRepr(s string) string {
	quote := byte('\'')
	if strings.IndexByte(s, '\'') >= 0 && strings.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	b := make([]byte, 0, len(s)+2)
	b = append(b, quote)
	for i := 0; i < len(s); {
		r, n := pystr.DecodeRune(s[i:])
		i += n
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
			b = appendHex(b, 'x', r, 2)
		case r < 0x7f || pystr.IsPrintable(r):
			b = pystr.AppendRune(b, r)
		default:
			b = appendEscape(b, r)
		}
	}
	return string(append(b, quote))
}

// Python repr of a bytes value
func BytesRepr(p []byte) string {
	quote := byte('\'')
	if strings.IndexByte(string(p), '\'') >= 0 && strings.IndexByte(string(p), '"') < 0 {
		quote = '"'
	}
	b := make([]byte, 0, len(p)+3)
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
			b = appendHex(b, 'x', rune(c), 2)
		default:
			b = append(b, c)
		}
	}
	return string(append(b, quote))
}

// Python ascii() of a value, its repr with every non ASCII code point escaped
func ASCII(v any) string { return EscapeNonASCII(Repr(v)) }

// Escapes non ASCII code points of WTF-8 text as backslash x, u or U sequences
func EscapeNonASCII(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		r, n := pystr.DecodeRune(s[i:])
		i += n
		if r < 0x80 {
			b = append(b, byte(r))
			continue
		}
		b = appendEscape(b, r)
	}
	return string(b)
}

func appendEscape(b []byte, r rune) []byte {
	switch {
	case r <= 0xff:
		return appendHex(b, 'x', r, 2)
	case r <= 0xffff:
		return appendHex(b, 'u', r, 4)
	}
	return appendHex(b, 'U', r, 8)
}

func appendHex(b []byte, kind byte, r rune, width int) []byte {
	h := strconv.FormatInt(int64(r), 16)
	b = append(b, '\\', kind)
	for range width - len(h) {
		b = append(b, '0')
	}
	return append(b, h...)
}

// Integer value of any Go integer kind or big.Int, false for bool and non integers
func Int(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case *big.Int:
		return x, x != nil
	case bool, nil:
		return nil, false
	case json.Number:
		if strings.ContainsAny(string(x), ".eE") {
			return nil, false
		}
		return new(big.Int).SetString(string(x), 10)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return big.NewInt(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return new(big.Int).SetUint64(rv.Uint()), true
	}
	return nil, false
}

// Float value of a Go float kind or a json.Number in float form
func floatOf(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case json.Number:
		if !strings.ContainsAny(string(x), ".eE") {
			return 0, false
		}
		f, err := strconv.ParseFloat(string(x), 64)
		return f, err == nil || isRangeErr(err)
	}
	return 0, false
}

func isRangeErr(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

// Python repr of a Go value modelling None, bool, int, float, str, bytes, list, tuple and dict
func Repr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return StrRepr(x)
	case []byte:
		return BytesRepr(x)
	case Reprer:
		return x.PyRepr()
	case Tuple:
		if len(x) == 1 {
			return "(" + Repr(x[0]) + ",)"
		}
		return "(" + joinRepr(len(x), func(i int) any { return x[i] }) + ")"
	case Mapping:
		return dictRepr(x.PyItems())
	}
	if n, ok := Int(v); ok {
		return n.String()
	}
	if f, ok := floatOf(v); ok {
		return Float(f)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return "[]"
		}
		return "[" + joinRepr(rv.Len(), func(i int) any { return rv.Index(i).Interface() }) + "]"
	case reflect.Map:
		return dictRepr(sortedItems(rv))
	}
	if s, ok := v.(fmt.Stringer); ok {
		return s.String()
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return fmt.Sprint(v)
}

func joinRepr(n int, at func(int) any) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = Repr(at(i))
	}
	return strings.Join(parts, ", ")
}

func dictRepr(items iter.Seq2[any, any]) string {
	var parts []string
	for k, v := range items {
		parts = append(parts, Repr(k)+": "+Repr(v))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Items of a Go map in sorted key order, strings by code point and other keys by repr
func sortedItems(rv reflect.Value) iter.Seq2[any, any] {
	keys := rv.MapKeys()
	text := make([]string, len(keys))
	for i, k := range keys {
		if k.Kind() == reflect.String {
			text[i] = k.String()
		} else {
			text[i] = Repr(k.Interface())
		}
	}
	idx := make([]int, len(keys))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return text[idx[a]] < text[idx[b]] })
	return func(yield func(any, any) bool) {
		for _, i := range idx {
			if !yield(keys[i].Interface(), rv.MapIndex(keys[i]).Interface()) {
				return
			}
		}
	}
}

// Sorted items of a string keyed map as a Mapping
func SortedMap[V any](m map[string]V) Mapping { return sortedMapping{reflect.ValueOf(m)} }

type sortedMapping struct{ rv reflect.Value }

func (m sortedMapping) PyItems() iter.Seq2[any, any] { return sortedItems(m.rv) }

// Python str() of a Go value, the text itself for str and the repr otherwise
func Str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case Strer:
		return x.PyStr()
	case Tuple, Mapping, []byte:
		return Repr(v)
	}
	if _, ok := Int(v); ok {
		return Repr(v)
	}
	if _, ok := floatOf(v); ok {
		return Repr(v)
	}
	if s, ok := v.(fmt.Stringer); ok {
		return s.String()
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return Repr(v)
}

// Python type name of a Go value
func TypeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case []byte:
		return "bytes"
	case TypeNamer:
		return x.PyTypeName()
	case Tuple:
		return "tuple"
	case Mapping:
		return "dict"
	}
	if _, ok := Int(v); ok {
		return "int"
	}
	if _, ok := floatOf(v); ok {
		return "float"
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		return "list"
	case reflect.Map:
		return "dict"
	case reflect.Pointer:
		return rv.Type().Elem().Name()
	}
	return rv.Type().Name()
}

// Python truth value of a Go value
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case Booler:
		return x.PyBool()
	case Mapping:
		for range x.PyItems() {
			return true
		}
		return false
	}
	if n, ok := Int(v); ok {
		return n.Sign() != 0
	}
	if f, ok := floatOf(v); ok {
		return f != 0
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return rv.Len() > 0
	case reflect.Pointer:
		return !rv.IsNil()
	}
	return true
}
