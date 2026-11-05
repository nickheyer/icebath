package pyjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Insertion ordered JSON object with dict semantics, a reassigned key keeping its first position
type Object struct {
	Keys []string
	Vals map[string]any
}

// Empty ordered object ready for Set
func NewObject() *Object { return &Object{Vals: map[string]any{}} }

// Value stored under key and whether the key exists
func (o *Object) Get(key string) (any, bool) {
	v, ok := o.Vals[key]
	return v, ok
}

// Stores v under key, appending the key only on first insertion
func (o *Object) Set(key string, v any) {
	if o.Vals == nil {
		o.Vals = map[string]any{}
	}
	if _, ok := o.Vals[key]; !ok {
		o.Keys = append(o.Keys, key)
	}
	o.Vals[key] = v
}

// Removes key and its position, reporting whether it was present
func (o *Object) Delete(key string) bool {
	if _, ok := o.Vals[key]; !ok {
		return false
	}
	delete(o.Vals, key)
	for i, k := range o.Keys {
		if k == key {
			o.Keys = append(o.Keys[:i:i], o.Keys[i+1:]...)
			break
		}
	}
	return true
}

// Number of keys
func (o *Object) Len() int { return len(o.Keys) }

// Writes keys in insertion order with encoding/json compact spacing and surrogates kept as code points
func (o *Object) MarshalJSON() ([]byte, error) {
	buf := []byte{'{'}
	for i, k := range o.Keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = AppendStringWTF8(buf, k)
		buf = append(buf, ':')
		val, err := EncodeWTF8(o.Vals[k])
		if err != nil {
			return nil, err
		}
		buf = append(buf, val...)
	}
	return append(buf, '}'), nil
}

// Python int of decimal JSON integer text, int64 when it fits and big.Int beyond
func ParseInt(text string) any {
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n
	}
	n, _ := new(big.Int).SetString(text, 10)
	return n
}

// ValueError json.dumps raises for a NaN or infinite float when allow_nan is false
var ErrOutOfRange = errors.New("Out of range float values are not JSON compliant")

// ValueError json.dumps raises when check_circular finds a container inside itself
var ErrCircular = errors.New("Circular reference detected")

// RecursionError for a default hook that keeps returning values needing default past the interpreter limit
var ErrRecursion = errors.New("maximum recursion depth exceeded while encoding a JSON object")

const maxDepth = 1000

// TypeError json.dumps raises for a value neither encodable nor converted by default
type TypeError struct{ Msg string }

func (e *TypeError) Error() string { return e.Msg }

// Python exception class name the error stands for
func (e *TypeError) PyTypeName() string { return "TypeError" }

type options struct {
	indent        *string
	itemSep       string
	keySep        string
	separators    bool
	ensureASCII   bool
	allowNaN      bool
	sortKeys      bool
	skipKeys      bool
	checkCircular bool
	def           func(any) (any, error)
}

// One json.dumps keyword argument
type Option func(*options)

// indent given as a count of spaces, zero meaning newlines with no indentation
func Indent(n int) Option {
	s := strings.Repeat(" ", max(n, 0))
	return func(o *options) { o.indent = &s }
}

// indent given as the literal per level string
func IndentText(s string) Option { return func(o *options) { o.indent = &s } }

// separators as the item and key separator pair
func Separators(item, key string) Option {
	return func(o *options) { o.itemSep, o.keySep, o.separators = item, key, true }
}

// ensure_ascii, true by default
func EnsureASCII(on bool) Option { return func(o *options) { o.ensureASCII = on } }

// allow_nan, true by default
func AllowNaN(on bool) Option { return func(o *options) { o.allowNaN = on } }

// sort_keys
func SortKeys(on bool) Option { return func(o *options) { o.sortKeys = on } }

// skipkeys, dropping entries whose key is not str int float bool or None
func SkipKeys(on bool) Option { return func(o *options) { o.skipKeys = on } }

// check_circular, true by default
func CheckCircular(on bool) Option { return func(o *options) { o.checkCircular = on } }

// default, called for values json cannot encode and its result encoded in their place
func Default(fn func(any) (any, error)) Option { return func(o *options) { o.def = fn } }

type encoder struct {
	options
	b     strings.Builder
	seen  map[uintptr]bool
	level int
	depth int
}

// json.dumps of v under the given keyword arguments, Python defaults for the rest
func Dumps(v any, opts ...Option) (string, error) {
	e := &encoder{options: options{ensureASCII: true, allowNaN: true, checkCircular: true}, seen: map[uintptr]bool{}}
	for _, opt := range opts {
		opt(&e.options)
	}
	if !e.separators {
		e.itemSep, e.keySep = ", ", ": "
		if e.indent != nil {
			e.itemSep = ","
		}
	}
	if err := e.encode(v); err != nil {
		return "", err
	}
	return e.b.String(), nil
}

// Writes one value in json.dumps form
func (e *encoder) encode(v any) error {
	switch x := v.(type) {
	case nil:
		e.b.WriteString("null")
	case bool:
		e.b.WriteString(strconv.FormatBool(x))
	case string:
		e.str(x)
	case json.Number:
		e.b.WriteString(x.String())
	case float64:
		return e.float(x)
	case float32:
		return e.float(float64(x))
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		fmt.Fprint(&e.b, x)
	case *big.Int:
		if x == nil {
			e.b.WriteString("null")
		} else {
			e.b.WriteString(x.String())
		}
	case *Object:
		if x == nil {
			e.b.WriteString("null")
			return nil
		}
		return e.container(reflect.ValueOf(x).Pointer(), func() error { return e.object(x) })
	case Object:
		return e.object(&x)
	case []any:
		return e.container(sliceID(reflect.ValueOf(x)), func() error {
			return e.list(len(x), func(i int) any { return x[i] })
		})
	default:
		return e.reflected(v)
	}
	return nil
}

// Encodes slices maps and pointers through reflection, handing anything else to default
func (e *encoder) reflected(v any) error {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice:
		if rv.IsNil() {
			e.b.WriteString("[]")
			return nil
		}
		return e.container(sliceID(rv), func() error {
			return e.list(rv.Len(), func(i int) any { return rv.Index(i).Interface() })
		})
	case reflect.Array:
		return e.list(rv.Len(), func(i int) any { return rv.Index(i).Interface() })
	case reflect.Map:
		if rv.IsNil() {
			e.b.WriteString("{}")
			return nil
		}
		return e.container(rv.Pointer(), func() error { return e.mapValue(rv) })
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			e.b.WriteString("null")
			return nil
		}
		return e.encode(rv.Elem().Interface())
	case reflect.String:
		e.str(rv.String())
		return nil
	case reflect.Bool:
		e.b.WriteString(strconv.FormatBool(rv.Bool()))
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.b.WriteString(strconv.FormatInt(rv.Int(), 10))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		e.b.WriteString(strconv.FormatUint(rv.Uint(), 10))
		return nil
	case reflect.Float32, reflect.Float64:
		return e.float(rv.Float())
	}
	if e.def == nil {
		return &TypeError{fmt.Sprintf("Object of type %T is not JSON serializable", v)}
	}
	id := uintptr(0)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		id = rv.Pointer()
	}
	e.depth++
	defer func() { e.depth-- }()
	if e.depth > maxDepth {
		return ErrRecursion
	}
	return e.container(id, func() error {
		out, err := e.def(v)
		if err != nil {
			return err
		}
		return e.encode(out)
	})
}

// Identity of a slice's backing array for the circular reference check
func sliceID(rv reflect.Value) uintptr {
	if rv.Len() == 0 {
		return 0
	}
	return rv.Pointer()
}

// Runs body with id marked as in progress, ValueError when it already is
func (e *encoder) container(id uintptr, body func() error) error {
	if !e.checkCircular || id == 0 {
		return body()
	}
	if e.seen[id] {
		return ErrCircular
	}
	e.seen[id] = true
	defer delete(e.seen, id)
	return body()
}

// Float text as repr, the JavaScript names for non finite values and ValueError for them when allow_nan is off
func (e *encoder) floatText(f float64) (string, error) {
	var text string
	switch {
	case math.IsNaN(f):
		text = "NaN"
	case math.IsInf(f, 1):
		text = "Infinity"
	case math.IsInf(f, -1):
		text = "-Infinity"
	default:
		return pyrepr.Float(f), nil
	}
	if !e.allowNaN {
		return "", fmt.Errorf("%w: %s", ErrOutOfRange, pyrepr.Float(f))
	}
	return text, nil
}

// Writes a float through floatText
func (e *encoder) float(f float64) error {
	text, err := e.floatText(f)
	e.b.WriteString(text)
	return err
}

// Newline and indent for the current nesting level
func (e *encoder) newline() {
	e.b.WriteByte('\n')
	for i := 0; i < e.level; i++ {
		e.b.WriteString(*e.indent)
	}
}

// Writes a list, one item per line when indenting
func (e *encoder) list(n int, at func(int) any) error {
	if n == 0 {
		e.b.WriteString("[]")
		return nil
	}
	e.b.WriteByte('[')
	if e.indent != nil {
		e.level++
		e.newline()
	}
	for i := 0; i < n; i++ {
		if i > 0 {
			e.b.WriteString(e.itemSep)
			if e.indent != nil {
				e.newline()
			}
		}
		if err := e.encode(at(i)); err != nil {
			return err
		}
	}
	if e.indent != nil {
		e.level--
		e.newline()
	}
	e.b.WriteByte(']')
	return nil
}

type member struct {
	key   string
	value any
}

// Writes an ordered object, sorted by key when sort_keys is set
func (e *encoder) object(o *Object) error {
	ms := make([]member, 0, len(o.Keys))
	for _, k := range o.Keys {
		ms = append(ms, member{k, o.Vals[k]})
	}
	if e.sortKeys {
		sort.SliceStable(ms, func(i, j int) bool { return ms[i].key < ms[j].key })
	}
	return e.members(ms)
}

// Writes a Go map with keys converted the way json.dumps converts dict keys, ordered by the original key values
func (e *encoder) mapValue(rv reflect.Value) error {
	keys := rv.MapKeys()
	kind := rv.Type().Key().Kind()
	sort.Slice(keys, func(i, j int) bool { return keyLess(keys[i], keys[j], kind) })
	ms := make([]member, 0, len(keys))
	for _, k := range keys {
		text, ok, err := e.keyText(k)
		if err != nil {
			return err
		}
		if ok {
			ms = append(ms, member{text, rv.MapIndex(k).Interface()})
		}
	}
	return e.members(ms)
}

// Ordering of map keys by their Go value so int keys sort numerically like Python sorted
func keyLess(a, b reflect.Value, kind reflect.Kind) bool {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return a.Int() < b.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return a.Uint() < b.Uint()
	case reflect.Float32, reflect.Float64:
		return a.Float() < b.Float()
	case reflect.Bool:
		return !a.Bool() && b.Bool()
	case reflect.String:
		return a.String() < b.String()
	}
	return fmt.Sprint(a.Interface()) < fmt.Sprint(b.Interface())
}

// Dict key as JSON text, false when skipkeys drops it and TypeError for unsupported key types
func (e *encoder) keyText(k reflect.Value) (string, bool, error) {
	if k.Kind() == reflect.Interface {
		if k.IsNil() {
			return "null", true, nil
		}
		k = k.Elem()
	}
	switch k.Kind() {
	case reflect.String:
		return k.String(), true, nil
	case reflect.Bool:
		return strconv.FormatBool(k.Bool()), true, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10), true, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(k.Uint(), 10), true, nil
	case reflect.Float32, reflect.Float64:
		text, err := e.floatText(k.Float())
		return text, err == nil, err
	}
	if e.skipKeys {
		return "", false, nil
	}
	return "", false, &TypeError{fmt.Sprintf("keys must be str, int, float, bool or None, not %s", k.Type())}
}

// Writes object members with key separators and indentation
func (e *encoder) members(ms []member) error {
	if len(ms) == 0 {
		e.b.WriteString("{}")
		return nil
	}
	e.b.WriteByte('{')
	if e.indent != nil {
		e.level++
		e.newline()
	}
	for i, m := range ms {
		if i > 0 {
			e.b.WriteString(e.itemSep)
			if e.indent != nil {
				e.newline()
			}
		}
		e.str(m.key)
		e.b.WriteString(e.keySep)
		if err := e.encode(m.value); err != nil {
			return err
		}
	}
	if e.indent != nil {
		e.level--
		e.newline()
	}
	e.b.WriteByte('}')
	return nil
}

// Writes a quoted string with json.dumps escapes, surrogates as \u escapes or raw WTF-8 code points
func (e *encoder) str(s string) {
	e.b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			cp, n := pystr.DecodeRune(s[i:])
			if e.ensureASCII {
				fmt.Fprintf(&e.b, `\u%04x`, cp)
			} else {
				e.b.WriteString(s[i : i+n])
			}
			i += n
			continue
		}
		i += size
		switch r {
		case '"':
			e.b.WriteString(`\"`)
		case '\\':
			e.b.WriteString(`\\`)
		case '\n':
			e.b.WriteString(`\n`)
		case '\r':
			e.b.WriteString(`\r`)
		case '\t':
			e.b.WriteString(`\t`)
		case '\b':
			e.b.WriteString(`\b`)
		case '\f':
			e.b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(&e.b, `\u%04x`, r)
			case e.ensureASCII && r > 0x7e:
				if r >= 0x10000 {
					r -= 0x10000
					fmt.Fprintf(&e.b, `\u%04x\u%04x`, 0xd800+(r>>10)&0x3ff, 0xdc00+r&0x3ff)
				} else {
					fmt.Fprintf(&e.b, `\u%04x`, r)
				}
			default:
				e.b.WriteRune(r)
			}
		}
	}
	e.b.WriteByte('"')
}

// Keys and values in insertion order so the object renders as a Python dict
func (o *Object) PyItems() iter.Seq2[any, any] {
	return func(yield func(any, any) bool) {
		if o == nil {
			return
		}
		for _, k := range o.Keys {
			if !yield(k, o.Vals[k]) {
				return
			}
		}
	}
}
