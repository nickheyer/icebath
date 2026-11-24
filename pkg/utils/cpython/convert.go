package cpythonutil

import (
	"cmp"
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"unsafe"
)

// Nesting depth the conversions refuse to go beyond
const maxDepth = 256

var zeroByte byte

// Pointer to the bytes of s for a C call, a valid pointer even for an empty string
func stringPointer(s string) unsafe.Pointer {
	if len(s) == 0 {
		return unsafe.Pointer(&zeroByte)
	}
	return unsafe.Pointer(unsafe.StringData(s))
}

// Converts a Go value into a new Python reference
func (g *GIL) ToPython(v any) (uintptr, error) { return g.toPython(v, 0) }

func (g *GIL) toPython(v any, depth int) (uintptr, error) {
	if depth > maxDepth {
		return 0, fmt.Errorf("%w: value nests deeper than %d", ErrUnconvertible, maxDepth)
	}
	switch x := v.(type) {
	case nil:
		return g.None(), nil
	case bool:
		return g.boolean(x), nil
	case int:
		return g.check(pyLongFromLongLong(int64(x)))
	case int8:
		return g.check(pyLongFromLongLong(int64(x)))
	case int16:
		return g.check(pyLongFromLongLong(int64(x)))
	case int32:
		return g.check(pyLongFromLongLong(int64(x)))
	case int64:
		return g.check(pyLongFromLongLong(x))
	case uint:
		return g.check(pyLongFromUnsignedLongLong(uint64(x)))
	case uint8:
		return g.check(pyLongFromUnsignedLongLong(uint64(x)))
	case uint16:
		return g.check(pyLongFromUnsignedLongLong(uint64(x)))
	case uint32:
		return g.check(pyLongFromUnsignedLongLong(uint64(x)))
	case uint64:
		return g.check(pyLongFromUnsignedLongLong(x))
	case *big.Int:
		return g.bigInt(x)
	case float32:
		return g.float(float64(x))
	case float64:
		return g.float(x)
	case string:
		return g.newUnicode(x)
	case []byte:
		return g.newBytes(x)
	case Borrowed:
		if x == 0 {
			return 0, fmt.Errorf("%w: Borrowed null pointer", ErrUnconvertible)
		}
		pyIncRef(uintptr(x))
		return uintptr(x), nil
	case *Ref:
		if x == nil {
			return g.None(), nil
		}
		pyIncRef(x.obj)
		return x.obj, nil
	case Func:
		if x == nil {
			return g.None(), nil
		}
		return g.NewFunc("function", x)
	case NamedFunc:
		if x.Fn == nil {
			return g.None(), nil
		}
		return g.NewFunc(x.Name, x.Fn)
	case Object:
		return g.NewObject(x)
	case []any:
		return listOf(g, x, depth)
	case []string:
		return listOf(g, x, depth)
	case map[string]any:
		return dictOf(g, x, depth)
	case map[string]string:
		return dictOf(g, x, depth)
	}
	return g.reflectToPython(reflect.ValueOf(v), depth)
}

// Converts what the type switch does not know: named scalar types, typed slices and arrays, string keyed maps and pointers
func (g *GIL) reflectToPython(rv reflect.Value, depth int) (uintptr, error) {
	switch rv.Kind() {
	case reflect.Bool:
		return g.boolean(rv.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return g.check(pyLongFromLongLong(rv.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return g.check(pyLongFromUnsignedLongLong(rv.Uint()))
	case reflect.Float32, reflect.Float64:
		return g.float(rv.Float())
	case reflect.String:
		return g.newUnicode(rv.String())
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return g.newBytes(rv.Bytes())
		}
		return g.reflectList(rv, depth)
	case reflect.Array:
		return g.reflectList(rv, depth)
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			return g.reflectDict(rv, depth)
		}
	case reflect.Pointer:
		if rv.IsNil() {
			return g.None(), nil
		}
		return g.toPython(rv.Elem().Interface(), depth+1)
	}
	return 0, fmt.Errorf("%w: Go %s to Python", ErrUnconvertible, rv.Type())
}

func (g *GIL) check(o uintptr) (uintptr, error) {
	if o == 0 {
		return 0, g.fetchError()
	}
	return o, nil
}

// New reference to True or False
func (g *GIL) boolean(b bool) uintptr {
	o := pyFalse
	if b {
		o = pyTrue
	}
	pyIncRef(o)
	return o
}

// Python int of a big integer, None for a nil pointer; hexadecimal text keeps clear of the decimal digit limit
func (g *GIL) bigInt(x *big.Int) (uintptr, error) {
	switch {
	case x == nil:
		return g.None(), nil
	case x.IsInt64():
		return g.check(pyLongFromLongLong(x.Int64()))
	case x.IsUint64():
		return g.check(pyLongFromUnsignedLongLong(x.Uint64()))
	}
	return g.check(pyLongFromString(x.Text(16), 0, 16))
}

// Float object holding f, bit for bit
func (g *GIL) float(f float64) (uintptr, error) { return g.check(pyFloatFromDouble(f)) }

// Value of a float or float subclass instance, which CPython reads from the object without running Python code
func (g *GIL) floatValue(o uintptr) (any, error) {
	v := pyFloatAsDouble(o)
	if v == -1 && pyErrOccurred() != 0 {
		return nil, g.fetchError()
	}
	return v, nil
}

// New str holding s; bytes that are not UTF-8 are kept through the surrogateescape handler so they convert back unchanged
func (g *GIL) newUnicode(s string) (uintptr, error) {
	p := stringPointer(s)
	if o := pyUnicodeFromStringAndSize(p, len(s)); o != 0 {
		return o, nil
	}
	pyErrClear()
	return g.check(pyUnicodeDecodeUTF8(p, len(s), "surrogateescape"))
}

func (g *GIL) newBytes(b []byte) (uintptr, error) {
	p := unsafe.Pointer(&zeroByte)
	if len(b) > 0 {
		p = unsafe.Pointer(unsafe.SliceData(b))
	}
	return g.check(pyBytesFromStringAndSize(p, len(b)))
}

func (g *GIL) newList(n int) (uintptr, error) { return g.check(pyListNew(n)) }

// List of converted items
func listOf[T any](g *GIL, items []T, depth int) (uintptr, error) {
	l, err := g.newList(len(items))
	if err != nil {
		return 0, err
	}
	for i, item := range items {
		o, err := g.toPython(item, depth+1)
		if err != nil {
			pyDecRef(l)
			return 0, err
		}
		pyListSetItem(l, i, o)
	}
	return l, nil
}

// Tuple of converted call arguments, a new reference
func (g *GIL) tuple(args []any) (uintptr, error) {
	t := pyTupleNew(len(args))
	if t == 0 {
		return 0, g.fetchError()
	}
	for i, a := range args {
		item, err := g.toPython(a, 1)
		if err != nil {
			pyDecRef(t)
			return 0, err
		}
		pyTupleSetItem(t, i, item)
	}
	return t, nil
}

// Dict of converted values, keys inserted in sorted order so the dict's iteration order does not depend on Go map order
func dictOf[V any](g *GIL, m map[string]V, depth int) (uintptr, error) {
	d, err := g.check(pyDictNew())
	if err != nil {
		return 0, err
	}
	if len(m) == 1 {
		for k, v := range m {
			if err := g.setItem(d, k, v, depth); err != nil {
				pyDecRef(d)
				return 0, err
			}
		}
		return d, nil
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if err := g.setItem(d, k, m[k], depth); err != nil {
			pyDecRef(d)
			return 0, err
		}
	}
	return d, nil
}

// Stores the converted value under the str key k in d
func (g *GIL) setItem(d uintptr, k string, v any, depth int) error {
	ko, err := g.newUnicode(k)
	if err != nil {
		return err
	}
	vo, err := g.toPython(v, depth+1)
	if err != nil {
		pyDecRef(ko)
		return err
	}
	if pyDictSetItem(d, ko, vo) != 0 {
		err = g.fetchError()
	}
	pyDecRef(ko)
	pyDecRef(vo)
	return err
}

func (g *GIL) reflectList(rv reflect.Value, depth int) (uintptr, error) {
	n := rv.Len()
	l, err := g.newList(n)
	if err != nil {
		return 0, err
	}
	for i := range n {
		o, err := g.toPython(rv.Index(i).Interface(), depth+1)
		if err != nil {
			pyDecRef(l)
			return 0, err
		}
		pyListSetItem(l, i, o)
	}
	return l, nil
}

func (g *GIL) reflectDict(rv reflect.Value, depth int) (uintptr, error) {
	d, err := g.check(pyDictNew())
	if err != nil {
		return 0, err
	}
	keys := rv.MapKeys()
	slices.SortFunc(keys, func(a, b reflect.Value) int { return cmp.Compare(a.String(), b.String()) })
	for _, k := range keys {
		if err := g.setItem(d, k.String(), rv.MapIndex(k).Interface(), depth); err != nil {
			pyDecRef(d)
			return 0, err
		}
	}
	return d, nil
}

// Converts a Python object to Go: None, bool, int, float, str, bytes and bytearray to Go values, dicts, lists and tuples by value, every other object as an owned *Ref
func (g *GIL) FromPython(o uintptr) (any, error) {
	if o == 0 {
		return nil, fmt.Errorf("%w: null object pointer", ErrUnconvertible)
	}
	return g.fromPython(o, 0)
}

func (g *GIL) fromPython(o uintptr, depth int) (any, error) {
	return g.convert(o, kindOf(o), depth)
}

// Go value of an object of a known kind: int64 or *big.Int for int, float64, string, []byte, map[string]any, []any, or *Ref
func (g *GIL) convert(o uintptr, k Kind, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: value nests deeper than %d", ErrUnconvertible, maxDepth)
	}
	switch k {
	case KindNone:
		return nil, nil
	case KindBool:
		return o == pyTrue, nil
	case KindInt:
		return g.integer(o)
	case KindFloat:
		return g.floatValue(o)
	case KindStr:
		return g.unicode(o)
	case KindBytes:
		return g.bytesValue(o)
	case KindByteArray:
		return g.byteArrayValue(o)
	case KindDict:
		return g.dictValue(o, depth)
	case KindList:
		return g.listValue(o, depth)
	case KindTuple:
		return g.tupleValue(o, depth)
	}
	return g.NewRef(o), nil
}

// Converts an object borrowed from a container; with a GIL a nested container is held while it converts, since a str() run for one of its dict keys may change what holds it
func (g *GIL) item(o uintptr, depth int) (any, error) {
	k := kindOf(o)
	if freeThreaded || !k.container() {
		return g.convert(o, k, depth)
	}
	pyIncRef(o)
	v, err := g.convert(o, k, depth)
	pyDecRef(o)
	return v, err
}

// Go integer of a Python int, int64 when it fits and *big.Int beyond
func (g *GIL) integer(o uintptr) (any, error) {
	v := pyLongAsLongLongAndOverflow(o, &g.out.overflow)
	if g.out.overflow == 0 {
		if v == -1 && pyErrOccurred() != 0 {
			return nil, g.fetchError()
		}
		return v, nil
	}
	// Hexadecimal text runs no Python code for int subclasses and is exempt from the decimal digit limit
	s := pyNumberToBase(o, 16)
	if s == 0 {
		return nil, g.fetchError()
	}
	text, err := g.unicode(s)
	pyDecRef(s)
	if err != nil {
		return nil, err
	}
	b, ok := new(big.Int).SetString(text, 0)
	if !ok {
		return nil, fmt.Errorf("%w: int %s does not parse", ErrUnconvertible, text)
	}
	return b, nil
}

// Text of a str; in a str holding lone surrogates each of U+DC80 to U+DCFF becomes the undecodable byte it escapes again and every other surrogate becomes U+FFFD
func (g *GIL) unicode(o uintptr) (string, error) {
	if p := pyUnicodeAsUTF8AndSize(o, &g.out.size); p != 0 {
		return string(unsafe.Slice((*byte)(cptr(p)), g.out.size)), nil
	}
	if !isStr(o) {
		return "", g.fetchError()
	}
	pyErrClear()
	b := pyUnicodeAsEncodedString(o, "utf-8", "surrogatepass")
	if b == 0 {
		return "", g.fetchError()
	}
	defer pyDecRef(b)
	return unescapeSurrogates(g.bytesOf(b)), nil
}

// Text of UTF-8 whose surrogates were encoded as three byte sequences ED A0-BF 80-BF, each escape surrogate turned back into its byte and any other surrogate into U+FFFD
func unescapeSurrogates(b []byte) string {
	var s strings.Builder
	s.Grow(len(b))
	for i := 0; i < len(b); i++ {
		// A valid UTF-8 sequence led by ED continues with 80 to 9F, so a higher second byte marks an encoded surrogate
		if b[i] == 0xED && i+2 < len(b) && b[i+1] >= 0xA0 {
			r := 0xD000 | rune(b[i+1]&0x3F)<<6 | rune(b[i+2]&0x3F)
			if r >= 0xDC80 && r <= 0xDCFF {
				s.WriteByte(byte(r))
			} else {
				s.WriteString("\uFFFD")
			}
			i += 2
			continue
		}
		s.WriteByte(b[i])
	}
	return s.String()
}

// Contents of a bytes object, borrowed from it
func (g *GIL) bytesOf(b uintptr) []byte {
	return unsafe.Slice((*byte)(cptr(pyBytesAsString(b))), pyBytesSize(b))
}

func (g *GIL) bytesValue(o uintptr) (any, error) {
	p := pyBytesAsString(o)
	if p == 0 {
		return nil, g.fetchError()
	}
	return copyOut(p, pyBytesSize(o)), nil
}

func (g *GIL) byteArrayValue(o uintptr) (any, error) {
	if freeThreaded {
		// Copied by CPython through the buffer protocol, which keeps other threads from resizing it meanwhile
		b := pyBytesFromObject(o)
		if b == 0 {
			return nil, g.fetchError()
		}
		defer pyDecRef(b)
		return g.bytesValue(b)
	}
	return copyOut(pyByteArrayAsString(o), pyByteArraySize(o)), nil
}

// Go copy of n bytes of C memory at p
func copyOut(p uintptr, n int) []byte {
	out := make([]byte, n)
	copy(out, unsafe.Slice((*byte)(cptr(p)), n))
	return out
}

// Walk over the stored key and value pairs of a dict or dict subclass, both borrowed: the dict's own storage with a GIL, on a free-threaded build a PyDict_Items snapshot CPython takes of that same storage under the dict's lock, which no other thread can change and which, unlike a copy of a subclass that overrides __iter__, never goes through its keys() and __getitem__
type dictWalk struct {
	g     *GIL
	dict  uintptr
	items uintptr
	n     int
	pos   int
}

// Starts a walk over the dict o, n the number of pairs it holds
func (g *GIL) walkDict(o uintptr) (dictWalk, error) {
	if !freeThreaded {
		return dictWalk{g: g, dict: o, n: max(pyDictSize(o), 0)}, nil
	}
	items := pyDictItems(o)
	if items == 0 {
		return dictWalk{}, g.fetchError()
	}
	return dictWalk{g: g, items: items, n: pyListSize(items)}, nil
}

// Next pair of the walk, ok false once every pair was seen
func (w *dictWalk) next() (k, v uintptr, ok bool) {
	if w.items == 0 {
		return w.g.dictNext(w.dict, &w.pos)
	}
	if w.pos >= w.n {
		return 0, 0, false
	}
	pair := pyListGetItem(w.items, w.pos)
	w.pos++
	return pyTupleGetItem(pair, 0), pyTupleGetItem(pair, 1), true
}

// Drops the snapshot of a free-threaded walk
func (w *dictWalk) close() {
	if w.items != 0 {
		pyDecRef(w.items)
	}
}

// Next key and value of a dict walk at *pos, both borrowed
func (g *GIL) dictNext(d uintptr, pos *int) (k, v uintptr, ok bool) {
	g.out.pos = *pos
	ok = pyDictNext(d, &g.out.pos, &g.out.key, &g.out.val) != 0
	*pos = g.out.pos
	return g.out.key, g.out.val, ok
}

func (g *GIL) dictValue(o uintptr, depth int) (map[string]any, error) {
	w, err := g.walkDict(o)
	if err != nil {
		return nil, err
	}
	defer w.close()
	out := make(map[string]any, w.n)
	for {
		k, v, ok := w.next()
		if !ok {
			return out, nil
		}
		if isStr(k) {
			key, err := g.unicode(k)
			if err != nil {
				return nil, err
			}
			if out[key], err = g.item(v, depth+1); err != nil {
				return nil, err
			}
			continue
		}
		// str() of the key runs Python code that may change the dict, so the pair is held across it
		pyIncRef(k)
		pyIncRef(v)
		key, err := g.Str(k)
		if err == nil {
			out[key], err = g.fromPython(v, depth+1)
		}
		pyDecRef(k)
		pyDecRef(v)
		if err != nil {
			return nil, err
		}
	}
}

func (g *GIL) listValue(o uintptr, depth int) ([]any, error) {
	if freeThreaded {
		// A tuple snapshot taken under the list's lock, items held by it however other threads change the list
		t := pyListAsTuple(o)
		if t == 0 {
			return nil, g.fetchError()
		}
		defer pyDecRef(t)
		return g.tupleValue(t, depth)
	}
	n := pyListSize(o)
	out := make([]any, n)
	for i := range n {
		item := pyListGetItem(o, i)
		if item == 0 {
			// Python code run for an earlier item shortened the list
			pyErrClear()
			return out[:i], nil
		}
		v, err := g.item(item, depth+1)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (g *GIL) tupleValue(o uintptr, depth int) ([]any, error) {
	n := pyTupleSize(o)
	out := make([]any, n)
	for i := range n {
		v, err := g.item(pyTupleGetItem(o, i), depth+1)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
