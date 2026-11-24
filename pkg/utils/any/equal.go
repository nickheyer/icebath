package anyutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"iter"
	"maps"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"unsafe"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Whether two values are deeply equal: numbers by value across integer and float types, a bool only to a bool, sequences element by element, maps and objects by their entries whatever their key order, values that contain themselves without looping
func Equal(a, b any) bool {
	var q equality
	return q.equal(a, b)
}

// Containers nested deeper than this are remembered pair by pair, so that a cycle ends the walk instead of the stack
const cycleDepth = 100

// One comparison in progress: its container depth and, once past cycleDepth, the container pairs already entered
type equality struct {
	depth int
	seen  map[containerPair]struct{}
}

// Identity of a map, slice or object: its address, and its length for a slice
type containerID struct {
	ptr unsafe.Pointer
	n   int
}

type containerPair struct{ a, b containerID }

func (q *equality) equal(a, b any) bool {
	ca, cb := classOf(a), classOf(b)
	if ca != cb {
		return false
	}
	switch ca {
	case classNil:
		return true
	case classBool:
		return boolOf(a) == boolOf(b)
	case classString:
		return stringOf(a) == stringOf(b)
	case classNumber:
		return numberOf(a).equal(numberOf(b))
	case classBytes:
		return bytes.Equal(bytesOf(a), bytesOf(b))
	case classList, classObject:
		q.depth++
		eq := q.depth > cycleDepth && q.entered(a, b)
		if !eq {
			if ca == classList {
				eq = q.listEqual(a, b)
			} else {
				eq = q.objectEqual(a, b)
			}
		}
		q.depth--
		return eq
	}
	return reflect.DeepEqual(a, b)
}

// Whether the pair of containers is already being compared further up, recording it when it is not; a pair seen again is a cycle, whose answer the first visit gives
func (q *equality) entered(a, b any) bool {
	ia, okA := identity(a)
	ib, okB := identity(b)
	if !okA || !okB {
		return false
	}
	key := containerPair{ia, ib}
	if _, ok := q.seen[key]; ok {
		return true
	}
	if q.seen == nil {
		q.seen = make(map[containerPair]struct{})
	}
	q.seen[key] = struct{}{}
	return false
}

// Identity of a non-empty map, slice or object, false for arrays and empty or nil containers, which cannot lead back to themselves
func identity(v any) (containerID, bool) {
	if o, ok := v.(*jsonutil.Object); ok {
		return containerID{ptr: unsafe.Pointer(o)}, o.Len() > 0
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Map:
		return containerID{ptr: rv.UnsafePointer(), n: -1}, rv.Len() > 0
	case reflect.Slice:
		return containerID{ptr: rv.UnsafePointer(), n: rv.Len()}, rv.Len() > 0
	}
	return containerID{}, false
}

type class uint8

const (
	classOther class = iota
	classNil
	classBool
	classString
	classNumber
	classBytes
	classList
	classObject
)

// Comparison class of a value, by type for the common shapes and by kind for the rest
func classOf(v any) class {
	switch x := v.(type) {
	case nil:
		return classNil
	case bool:
		return classBool
	case string:
		return classString
	case int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8, uintptr, float64, float32, json.Number, *big.Int:
		return classNumber
	case []byte:
		return classBytes
	case []any, []string:
		return classList
	case map[string]any:
		return classObject
	case *jsonutil.Object:
		// a nil object is JSON null, the way it encodes
		if x == nil {
			return classNil
		}
		return classObject
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Bool:
		return classBool
	case reflect.String:
		return classString
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return classNumber
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return classBytes
		}
		return classList
	case reflect.Array:
		return classList
	case reflect.Map:
		return classObject
	}
	return classOther
}

func boolOf(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return reflect.ValueOf(v).Bool()
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return reflect.ValueOf(v).String()
}

func bytesOf(v any) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	return reflect.ValueOf(v).Bytes()
}

type numberForm uint8

const (
	formInt numberForm = iota
	formUint
	formFloat
	formBig
)

// A number in the narrowest exact form: int64, uint64 above the int64 range, float64, or a *big.Int beyond both
type number struct {
	form numberForm
	i    int64
	u    uint64
	f    float64
	b    *big.Int
}

func numberOf(v any) number {
	switch x := v.(type) {
	case int:
		return number{form: formInt, i: int64(x)}
	case int64:
		return number{form: formInt, i: x}
	case int32:
		return number{form: formInt, i: int64(x)}
	case int16:
		return number{form: formInt, i: int64(x)}
	case int8:
		return number{form: formInt, i: int64(x)}
	case uint:
		return unsignedNumber(uint64(x))
	case uint64:
		return unsignedNumber(x)
	case uint32:
		return number{form: formInt, i: int64(x)}
	case uint16:
		return number{form: formInt, i: int64(x)}
	case uint8:
		return number{form: formInt, i: int64(x)}
	case uintptr:
		return unsignedNumber(uint64(x))
	case float64:
		return number{form: formFloat, f: x}
	case float32:
		return number{form: formFloat, f: float64(x)}
	case json.Number:
		if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return number{form: formInt, i: n}
		}
		if b, ok := new(big.Int).SetString(string(x), 10); ok {
			return bigNumber(b)
		}
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			f = math.NaN()
		}
		return number{form: formFloat, f: f}
	case *big.Int:
		if x == nil {
			return number{form: formFloat, f: math.NaN()}
		}
		return bigNumber(x)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return number{form: formInt, i: rv.Int()}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return unsignedNumber(rv.Uint())
	}
	return number{form: formFloat, f: rv.Float()}
}

func unsignedNumber(u uint64) number {
	if u <= math.MaxInt64 {
		return number{form: formInt, i: int64(u)}
	}
	return number{form: formUint, u: u}
}

func bigNumber(b *big.Int) number {
	switch {
	case b.IsInt64():
		return number{form: formInt, i: b.Int64()}
	case b.IsUint64():
		return number{form: formUint, u: b.Uint64()}
	}
	return number{form: formBig, b: b}
}

// Exact equality of two numbers in any of their forms, NaN equal to nothing
func (x number) equal(y number) bool {
	if x.form > y.form {
		x, y = y, x
	}
	switch x.form {
	case formInt:
		switch y.form {
		case formInt:
			return x.i == y.i
		case formFloat:
			return y.f == math.Trunc(y.f) && y.f >= minInt64Float && y.f < maxInt64Float && int64(y.f) == x.i
		}
		return false
	case formUint:
		switch y.form {
		case formUint:
			return x.u == y.u
		case formFloat:
			return y.f == math.Trunc(y.f) && y.f >= maxInt64Float && y.f < 2*maxInt64Float && uint64(y.f) == x.u
		}
		return false
	case formFloat:
		switch y.form {
		case formFloat:
			return x.f == y.f
		case formBig:
			if math.IsNaN(x.f) || math.IsInf(x.f, 0) || x.f != math.Trunc(x.f) {
				return false
			}
			return new(big.Float).SetFloat64(x.f).Cmp(new(big.Float).SetInt(y.b)) == 0
		}
		return false
	}
	return x.b.Cmp(y.b) == 0
}

// Element-wise equality of two arrays or slices
func (q *equality) listEqual(a, b any) bool {
	if x, ok := a.([]any); ok {
		if y, ok := b.([]any); ok {
			if len(x) != len(y) {
				return false
			}
			for i := range x {
				if !q.equal(x[i], y[i]) {
					return false
				}
			}
			return true
		}
	}
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	if ra.Len() != rb.Len() {
		return false
	}
	for i := range ra.Len() {
		if !q.equal(ra.Index(i).Interface(), rb.Index(i).Interface()) {
			return false
		}
	}
	return true
}

// Entry-wise equality of two maps or objects, whatever their key order
func (q *equality) objectEqual(a, b any) bool {
	if x, ok := a.(map[string]any); ok {
		if y, ok := b.(map[string]any); ok {
			if len(x) != len(y) {
				return false
			}
			for k, va := range x {
				vb, ok := y[k]
				if !ok || !q.equal(va, vb) {
					return false
				}
			}
			return true
		}
	}
	va, vb := viewOf(a), viewOf(b)
	if va.size() != vb.size() || va.textKeys() != vb.textKeys() {
		return false
	}
	if va.textKeys() {
		for k, v := range va.entries() {
			other, ok := vb.get(k)
			if !ok || !q.equal(v, other) {
				return false
			}
		}
		return true
	}
	if va.rv.Type().Key() != vb.rv.Type().Key() {
		return false
	}
	it := va.rv.MapRange()
	for it.Next() {
		other := vb.rv.MapIndex(it.Key())
		if !other.IsValid() || !q.equal(it.Value().Interface(), other.Interface()) {
			return false
		}
	}
	return true
}

// Read access to a plain map, an object, or a map of another type through reflection
type view struct {
	m   map[string]any
	obj *jsonutil.Object
	rv  reflect.Value
}

func viewOf(v any) view {
	switch x := v.(type) {
	case map[string]any:
		return view{m: x}
	case *jsonutil.Object:
		return view{obj: x}
	}
	return view{rv: reflect.ValueOf(v)}
}

func (w view) size() int {
	switch {
	case w.obj != nil:
		return w.obj.Len()
	case w.rv.IsValid():
		return w.rv.Len()
	}
	return len(w.m)
}

// Whether the keys are text, so that maps of different key types can still be compared
func (w view) textKeys() bool {
	return !w.rv.IsValid() || w.rv.Type().Key().Kind() == reflect.String
}

func (w view) entries() iter.Seq2[string, any] {
	switch {
	case w.obj != nil:
		return w.obj.All()
	case w.rv.IsValid():
		return func(yield func(string, any) bool) {
			it := w.rv.MapRange()
			for it.Next() {
				if !yield(it.Key().String(), it.Value().Interface()) {
					return
				}
			}
		}
	}
	return maps.All(w.m)
}

func (w view) get(key string) (any, bool) {
	switch {
	case w.obj != nil:
		return w.obj.Get(key)
	case w.rv.IsValid():
		v := w.rv.MapIndex(reflect.ValueOf(key).Convert(w.rv.Type().Key()))
		if !v.IsValid() {
			return nil, false
		}
		return v.Interface(), true
	}
	v, ok := w.m[key]
	return v, ok
}
