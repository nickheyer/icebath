package pyhost

import (
	"fmt"
	"math/big"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"unsafe"
)

// Borrowed passes an existing object pointer as an argument, the callee takes its own reference
type Borrowed uintptr

// Ref is an owned reference held by Go, usable from any goroutine and released under the GIL
type Ref struct {
	h       *Host
	obj     uintptr
	once    sync.Once
	cleanup runtime.Cleanup
}

// Takes a new reference to o for Go to hold
func (g *GIL) NewRef(o uintptr) *Ref {
	pyIncRef(o)
	return g.adopt(o)
}

// Wraps a new reference without incrementing it
func (g *GIL) adopt(o uintptr) *Ref {
	r := &Ref{h: g.h, obj: o}
	h := g.h
	r.cleanup = runtime.AddCleanup(r, func(p uintptr) { h.queueRelease(p) }, o)
	return r
}

// Borrowed pointer of the referenced object, valid while the Ref is held
func (r *Ref) Ptr() uintptr { return r.obj }

// Drops Go's reference at the next GIL entry
func (r *Ref) Release() {
	r.once.Do(func() {
		r.cleanup.Stop()
		r.h.queueRelease(r.obj)
	})
}

// Calls the referenced callable from any goroutine and converts the result to Go
func (r *Ref) Call(args []any, kwargs map[string]any) (any, error) {
	var out any
	err := r.h.Do(func(g *GIL) error {
		res, err := g.Call(r.obj, args, kwargs)
		if err != nil {
			return err
		}
		defer pyDecRef(res)
		out, err = g.FromPython(res)
		return err
	})
	return out, err
}

const maxDepth = 256

// Converts a Go value into a new Python reference
func (g *GIL) ToPython(v any) (uintptr, error) { return g.toPython(v, 0) }

func (g *GIL) toPython(v any, depth int) (uintptr, error) {
	if depth > maxDepth {
		return 0, fmt.Errorf("value nests deeper than %d", maxDepth)
	}
	switch x := v.(type) {
	case nil:
		pyIncRef(pyNone)
		return pyNone, nil
	case bool:
		o := pyFalse
		if x {
			o = pyTrue
		}
		pyIncRef(o)
		return o, nil
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
		return g.check(pyLongFromString(x.String(), 0, 10))
	case float32:
		return g.float(float64(x))
	case float64:
		return g.float(x)
	case string:
		return g.newUnicode(x)
	case []byte:
		p := unsafe.Pointer(&zeroByte)
		if len(x) > 0 {
			p = unsafe.Pointer(&x[0])
		}
		return g.check(pyBytesFromStringAndSize(p, len(x)))
	case Borrowed:
		pyIncRef(uintptr(x))
		return uintptr(x), nil
	case *Ref:
		pyIncRef(x.obj)
		return x.obj, nil
	case Func:
		return g.NewFunc("function", x)
	case NamedFunc:
		return g.NewFunc(x.Name, x.Fn)
	case Object:
		return g.NewObject(x)
	case []any:
		return g.list(len(x), func(i int) any { return x[i] }, depth)
	case map[string]any:
		return g.dict(x, depth)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		return g.list(rv.Len(), func(i int) any { return rv.Index(i).Interface() }, depth)
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			m := make(map[string]any, rv.Len())
			for _, k := range rv.MapKeys() {
				m[k.String()] = rv.MapIndex(k).Interface()
			}
			return g.dict(m, depth)
		}
	case reflect.Pointer:
		if rv.IsNil() {
			pyIncRef(pyNone)
			return pyNone, nil
		}
	}
	return 0, fmt.Errorf("no Python conversion for Go %T", v)
}

var zeroByte byte

func (g *GIL) check(o uintptr) (uintptr, error) {
	if o == 0 {
		return 0, g.fetchError()
	}
	return o, nil
}

// Builds a float through its shortest decimal form so no float travels in a register
func (g *GIL) float(f float64) (uintptr, error) {
	s, err := g.newUnicode(strconv.FormatFloat(f, 'g', -1, 64))
	if err != nil {
		return 0, err
	}
	defer pyDecRef(s)
	return g.check(pyFloatFromString(s))
}

func (g *GIL) newUnicode(s string) (uintptr, error) {
	p := unsafe.Pointer(&zeroByte)
	if len(s) > 0 {
		p = unsafe.Pointer(unsafe.StringData(s))
	}
	return g.check(pyUnicodeFromStringAndSize(p, len(s)))
}

func (g *GIL) list(n int, at func(int) any, depth int) (uintptr, error) {
	l := pyListNew(n)
	if l == 0 {
		return 0, g.fetchError()
	}
	for i := 0; i < n; i++ {
		item, err := g.toPython(at(i), depth+1)
		if err != nil {
			pyDecRef(l)
			return 0, err
		}
		pyListSetItem(l, i, item)
	}
	return l, nil
}

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

func (g *GIL) dict(m map[string]any, depth int) (uintptr, error) {
	d := pyDictNew()
	if d == 0 {
		return 0, g.fetchError()
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ko, err := g.newUnicode(k)
		if err != nil {
			pyDecRef(d)
			return 0, err
		}
		vo, err := g.toPython(m[k], depth+1)
		if err != nil {
			pyDecRef(ko)
			pyDecRef(d)
			return 0, err
		}
		rc := pyDictSetItem(d, ko, vo)
		pyDecRef(ko)
		pyDecRef(vo)
		if rc != 0 {
			pyDecRef(d)
			return 0, g.fetchError()
		}
	}
	return d, nil
}

// Converts a Python object to Go, containers by value and every other object as an owned *Ref
func (g *GIL) FromPython(o uintptr) (any, error) { return g.fromPython(o, 0) }

func (g *GIL) fromPython(o uintptr, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("value nests deeper than %d", maxDepth)
	}
	if o == pyNone {
		return nil, nil
	}
	t := pyType(o)
	if t == pyBoolType {
		return o == pyTrue, nil
	}
	flags := pyTypeGetFlags(t)
	switch {
	case flags&tpflagsLong != 0:
		var overflow int32
		v := pyLongAsLongLongAndOverflow(o, &overflow)
		if overflow == 0 {
			if v == -1 && pyErrOccurred() != 0 {
				return nil, g.fetchError()
			}
			return v, nil
		}
		s, err := g.Str(o)
		if err != nil {
			return nil, err
		}
		b, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return nil, fmt.Errorf("int %q does not parse", s)
		}
		return b, nil
	case flags&tpflagsUnicode != 0:
		return g.unicode(o)
	case flags&tpflagsBytes != 0:
		p := pyBytesAsString(o)
		if p == 0 {
			return nil, g.fetchError()
		}
		return append([]byte(nil), unsafe.Slice((*byte)(cptr(p)), pyBytesSize(o))...), nil
	case flags&tpflagsDict != 0:
		out := map[string]any{}
		var pos int
		var k, v uintptr
		for pyDictNext(o, &pos, &k, &v) != 0 {
			var key string
			var err error
			if pyTypeGetFlags(pyType(k))&tpflagsUnicode != 0 {
				key, err = g.unicode(k)
			} else {
				key, err = g.Str(k)
			}
			if err != nil {
				return nil, err
			}
			if out[key], err = g.fromPython(v, depth+1); err != nil {
				return nil, err
			}
		}
		return out, nil
	case flags&tpflagsList != 0:
		n := pyListSize(o)
		out := make([]any, n)
		for i := 0; i < n; i++ {
			v, err := g.fromPython(pyListGetItem(o, i), depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case flags&tpflagsTuple != 0:
		n := pyTupleSize(o)
		out := make([]any, n)
		for i := 0; i < n; i++ {
			v, err := g.fromPython(pyTupleGetItem(o, i), depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	if t == pyFloatType || pyObjectIsInstance(o, pyFloatType) == 1 {
		return *(*float64)(cptr(o + floatValOffset)), nil
	}
	if t == pyByteArrayType {
		p := pyByteArrayAsString(o)
		return append([]byte(nil), unsafe.Slice((*byte)(cptr(p)), pyByteArraySize(o))...), nil
	}
	return g.NewRef(o), nil
}

const (
	tpflagsLong    = 1 << 24
	tpflagsList    = 1 << 25
	tpflagsTuple   = 1 << 26
	tpflagsBytes   = 1 << 27
	tpflagsUnicode = 1 << 28
	tpflagsDict    = 1 << 29
)

func (g *GIL) unicode(o uintptr) (string, error) {
	var n int
	p := pyUnicodeAsUTF8AndSize(o, &n)
	if p == 0 {
		return "", g.fetchError()
	}
	return string(unsafe.Slice((*byte)(cptr(p)), n)), nil
}

// Reads an attribute of the referenced object from any goroutine
func (r *Ref) GetAttr(name string) (any, error) {
	var out any
	err := r.h.Do(func(g *GIL) error {
		v, err := g.Attr(r.obj, name)
		if err != nil {
			return err
		}
		defer pyDecRef(v)
		out, err = g.FromPython(v)
		return err
	})
	return out, err
}

// Calls a method of the referenced object from any goroutine
func (r *Ref) CallMethod(name string, args []any, kwargs map[string]any) (any, error) {
	var out any
	err := r.h.Do(func(g *GIL) error {
		v, err := g.CallMethod(r.obj, name, args, kwargs)
		if err != nil {
			return err
		}
		defer pyDecRef(v)
		out, err = g.FromPython(v)
		return err
	})
	return out, err
}

// Type name of the referenced object
func (r *Ref) TypeName() string {
	var out string
	_ = r.h.Do(func(g *GIL) error {
		t := pyObjectType(r.obj)
		defer pyDecRef(t)
		out = g.qualifiedName(t)
		return nil
	})
	return out
}

// Reads an attribute while the caller already holds the GIL
func (r *Ref) GetAttrLocked(g *GIL, name string) (any, error) {
	v, err := g.Attr(r.obj, name)
	if err != nil {
		return nil, err
	}
	defer pyDecRef(v)
	return g.FromPython(v)
}

// Calls the referenced callable on the current thread while the GIL is already held
func (r *Ref) CallLocked(g *GIL, args []any, kwargs map[string]any) (any, error) {
	res, err := g.Call(r.obj, args, kwargs)
	if err != nil {
		return nil, err
	}
	defer pyDecRef(res)
	return g.FromPython(res)
}
