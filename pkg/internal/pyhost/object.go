package pyhost

import (
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Func is a Go function callable from Python with arguments converted to Go values
type Func func(g *GIL, args []any, kwargs map[string]any) (any, error)

// RawFunc receives the argument tuple and keyword dict as borrowed pointers and returns a new reference
type RawFunc func(g *GIL, args, kwargs uintptr) (uintptr, error)

// NamedFunc gives a Func its Python __name__
type NamedFunc struct {
	Name string
	Fn   Func
}

// Object is a Go value exposed to Python, attribute lookup reaches GetAttr
type Object interface {
	TypeName() (module, name string)
	GetAttr(g *GIL, name string) (value any, ok bool, err error)
}

// Every Go function and Go object lives behind one handle stored in its Python object
type handleEntry struct {
	obj   Object
	raw   RawFunc
	mu    sync.Mutex
	cache map[string]uintptr
}

var (
	handlesMu  sync.RWMutex
	handles    = map[uint64]*handleEntry{}
	handleSeq  atomic.Uint64
	typesMu    sync.Mutex
	goTypes    = map[string]uintptr{}
	methodDefs = map[string]uintptr{}
)

// The three C entry points into Go, created once per process whatever the number of plugins or methods
var (
	callbacksOnce sync.Once
	cbMethod      uintptr
	cbGetattro    uintptr
	cbDealloc     uintptr
)

const (
	slotTpDealloc         = 52
	slotTpGetattro        = 58
	slotTpFree            = 74
	methVarargsKeywords   = 0x0001 | 0x0002
	tpflagsDefault        = 1 << 18
	tpflagsDisallowCreate = 1 << 7
)

func ensureCallbacks() {
	callbacksOnce.Do(func() {
		cbMethod = purego.NewCallback(methodTrampoline)
		cbGetattro = purego.NewCallback(getattroTrampoline)
		cbDealloc = purego.NewCallback(deallocTrampoline)
	})
}

// Converts a Go panic into a Python RuntimeError so it never unwinds through C frames
func guard(ret *uintptr) {
	if p := recover(); p != nil {
		pyErrSetString(excRuntimeError, fmt.Sprintf("Go panic in host callback: %v\n%s", p, debug.Stack()))
		*ret = 0
	}
}

func readHandle(o uintptr) uint64 { return *(*uint64)(cptr(o + objectBasicSize)) }

func lookupHandle(id uint64) *handleEntry {
	handlesMu.RLock()
	defer handlesMu.RUnlock()
	return handles[id]
}

// PyCFunctionWithKeywords shared by every Go function, the function is found through the handle in self
func methodTrampoline(self, args, kwargs uintptr) (ret uintptr) {
	defer guard(&ret)
	g := &GIL{h: current}
	e := lookupHandle(readHandle(self))
	if e == nil || e.raw == nil {
		pyErrSetString(excRuntimeError, "Go function was released")
		return 0
	}
	r, err := e.raw(g, args, kwargs)
	if err != nil {
		g.raise(err)
		return 0
	}
	return r
}

// Getattro of Go objects, public names go to the Go implementation and dunder names to generic lookup
func getattroTrampoline(self, name uintptr) (ret uintptr) {
	defer guard(&ret)
	g := &GIL{h: current}
	e := lookupHandle(readHandle(self))
	if e == nil || e.obj == nil {
		return pyObjectGenericGetAttr(self, name)
	}
	attr, err := g.unicode(name)
	if err != nil {
		g.raise(err)
		return 0
	}
	if len(attr) > 1 && attr[0] == '_' && attr[1] == '_' {
		return pyObjectGenericGetAttr(self, name)
	}
	e.mu.Lock()
	cached, hit := e.cache[attr]
	e.mu.Unlock()
	if hit {
		pyIncRef(cached)
		return cached
	}
	v, ok, err := e.obj.GetAttr(g, attr)
	if err != nil {
		g.raise(err)
		return 0
	}
	if !ok {
		_, typeName := e.obj.TypeName()
		pyErrSetString(excAttributeError(g), fmt.Sprintf("'%s' object has no attribute '%s'", typeName, attr))
		return 0
	}
	o, err := g.ToPython(v)
	if err != nil {
		g.raise(err)
		return 0
	}
	switch v.(type) {
	case Func, NamedFunc:
		e.mu.Lock()
		if e.cache == nil {
			e.cache = map[string]uintptr{}
		}
		if prev, dup := e.cache[attr]; dup {
			e.mu.Unlock()
			pyDecRef(o)
			pyIncRef(prev)
			return prev
		}
		pyIncRef(o)
		e.cache[attr] = o
		e.mu.Unlock()
	}
	return o
}

// Dealloc of Go handle objects, drops the Go side and frees the object through its type
func deallocTrampoline(self uintptr) (ret uintptr) {
	defer guard(&ret)
	id := readHandle(self)
	handlesMu.Lock()
	e := handles[id]
	delete(handles, id)
	handlesMu.Unlock()
	if e != nil {
		e.mu.Lock()
		for _, o := range e.cache {
			pyDecRef(o)
		}
		e.cache = nil
		e.mu.Unlock()
	}
	tp := pyType(self)
	purego.SyscallN(pyTypeGetSlot(tp, slotTpFree), self)
	pyDecRef(tp)
	return 0
}

// C string that lives for the rest of the process
func permanentCString(s string) uintptr {
	p := pyMemRawMalloc(uintptr(len(s) + 1))
	b := unsafe.Slice((*byte)(cptr(p)), len(s)+1)
	copy(b, s)
	b[len(s)] = 0
	return p
}

// PyType_Spec followed by its slot array in one C allocation
func newSpec(name string, basicSize uintptr, flags uint32, slots [][2]uintptr) uintptr {
	n := len(slots) + 1
	mem := pyMemRawMalloc(uintptr(32 + 16*n))
	clear(unsafe.Slice((*byte)(cptr(mem)), 32+16*n))
	*(*uintptr)(cptr(mem)) = permanentCString(name)
	*(*int32)(cptr(mem + 8)) = int32(basicSize)
	*(*int32)(cptr(mem + 12)) = 0
	*(*uint32)(cptr(mem + 16)) = flags
	slotArr := mem + 32
	*(*uintptr)(cptr(mem + 24)) = slotArr
	for i, s := range slots {
		*(*int32)(cptr(slotArr + uintptr(16*i))) = int32(s[0])
		*(*uintptr)(cptr(slotArr + uintptr(16*i) + 8)) = s[1]
	}
	return mem
}

// Heap type for Go handle objects of one Python type name, created once per name
func (g *GIL) goType(module, name string) (uintptr, error) {
	key := module + "." + name
	typesMu.Lock()
	defer typesMu.Unlock()
	if t, ok := goTypes[key]; ok {
		return t, nil
	}
	spec := newSpec(name, objectBasicSize+8, tpflagsDefault|tpflagsDisallowCreate, [][2]uintptr{
		{slotTpGetattro, cbGetattro}, {slotTpDealloc, cbDealloc},
	})
	t := pyTypeFromSpec(spec)
	if t == 0 {
		return 0, g.fetchError()
	}
	if err := g.SetAttr(t, "__module__", module); err != nil {
		return 0, err
	}
	goTypes[key] = t
	return t, nil
}

func (g *GIL) newHandleObject(module, name string, e *handleEntry) (uintptr, error) {
	t, err := g.goType(module, name)
	if err != nil {
		return 0, err
	}
	o := pyTypeGenericAlloc(t, 0)
	if o == 0 {
		return 0, g.fetchError()
	}
	id := handleSeq.Add(1)
	handlesMu.Lock()
	handles[id] = e
	handlesMu.Unlock()
	*(*uint64)(cptr(o + objectBasicSize)) = id
	return o, nil
}

// Python object whose attribute lookup is answered by impl, a new reference
func (g *GIL) NewObject(impl Object) (uintptr, error) {
	module, name := impl.TypeName()
	return g.newHandleObject(module, name, &handleEntry{obj: impl})
}

// Native callable named name that runs fn, a new reference
func (g *GIL) NewFunc(name string, fn Func) (uintptr, error) {
	return g.NewRawFunc(name, func(g *GIL, args, kwargs uintptr) (uintptr, error) {
		a, err := g.FromPython(args)
		if err != nil {
			return 0, err
		}
		var kw map[string]any
		if kwargs != 0 {
			v, err := g.FromPython(kwargs)
			if err != nil {
				return 0, err
			}
			kw = v.(map[string]any)
		}
		out, err := fn(g, a.([]any), kw)
		if err != nil {
			return 0, err
		}
		return g.ToPython(out)
	})
}

// Native callable over raw pointers, the PyMethodDef is shared per name and self carries the handle
func (g *GIL) NewRawFunc(name string, fn RawFunc) (uintptr, error) {
	self, err := g.newHandleObject("icebath", "GoFunctionHandle", &handleEntry{raw: fn})
	if err != nil {
		return 0, err
	}
	defer pyDecRef(self)
	typesMu.Lock()
	def, ok := methodDefs[name]
	if !ok {
		def = pyMemRawMalloc(32)
		*(*uintptr)(cptr(def)) = permanentCString(name)
		*(*uintptr)(cptr(def + 8)) = cbMethod
		*(*int32)(cptr(def + 16)) = methVarargsKeywords
		*(*int32)(cptr(def + 20)) = 0
		*(*uintptr)(cptr(def + 24)) = 0
		methodDefs[name] = def
	}
	typesMu.Unlock()
	f := pyCFunctionNewEx(def, self, 0)
	if f == 0 {
		return 0, g.fetchError()
	}
	return f, nil
}

var errNoLoop = errors.New("asyncio loop is not running")
