package cpythonutil

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// Go side of a handle object: the Go function or object it stands for and the attribute values cached for it
type handleEntry struct {
	obj   Object
	raw   RawFunc
	mu    sync.Mutex
	cache map[string]uintptr
}

// Every Go function and Go object lives behind one handle id stored in its Python object
var (
	handlesMu sync.RWMutex
	handles   = map[uint64]*handleEntry{}
	handleSeq atomic.Uint64
)

// The three C entry points into Go, created once per process whatever the number of callables and objects
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
		cbMethod = newCallback(methodTrampoline)
		cbGetattro = newCallback(getattroTrampoline)
		cbDealloc = newCallback(deallocTrampoline)
	})
}

// Turns a Go panic into a Python RuntimeError so it never unwinds through C frames, its stack logged
func (g *GIL) guard(ret *uintptr) {
	if p := recover(); p != nil {
		logPanic(p)
		g.setError(excRuntimeError, fmt.Sprintf("Go panic in host callback: %v", p))
		*ret = 0
	}
}

func logPanic(p any) {
	slog.Default().With("logger", "cpython").Error("Go panic in host callback", "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
}

func readHandle(o uintptr) uint64 { return *(*uint64)(cptr(o + objectBasicSize)) }

func lookupHandle(id uint64) *handleEntry {
	handlesMu.RLock()
	defer handlesMu.RUnlock()
	return handles[id]
}

// PyCFunctionWithKeywords shared by every Go function, the function found through the handle in self
func methodTrampoline(self, args, kwargs uintptr) (ret uintptr) {
	g := &GIL{h: current.Load()}
	defer g.guard(&ret)
	e := lookupHandle(readHandle(self))
	if e == nil || e.raw == nil {
		g.setError(excRuntimeError, "Go function was released")
		return 0
	}
	r, err := e.raw(g, args, kwargs)
	switch {
	case err != nil:
		if r != 0 {
			pyDecRef(r)
		}
		g.raise(err)
		return 0
	case r == 0 && pyErrOccurred() == 0:
		g.setError(excSystemError, "Go function returned no object and no error")
	}
	return r
}

// Getattro of Go objects, public names going to the Go implementation and dunder names to generic lookup
func getattroTrampoline(self, name uintptr) (ret uintptr) {
	g := &GIL{h: current.Load()}
	defer g.guard(&ret)
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
		g.setError(excAttributeError, fmt.Sprintf("'%s' object has no attribute '%s'", typeName, attr))
		return 0
	}
	o, err := g.ToPython(v)
	if err != nil {
		g.raise(err)
		return 0
	}
	switch v.(type) {
	case Func, NamedFunc:
		// Function attributes are cached so every lookup of a name returns the same callable object
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

// Dealloc of Go handle objects, which drops the Go side and frees the object through its type
func deallocTrampoline(self uintptr) (ret uintptr) {
	// A dealloc must not raise, so a panic here is only logged
	defer func() {
		if p := recover(); p != nil {
			logPanic(p)
		}
	}()
	id := readHandle(self)
	handlesMu.Lock()
	e := handles[id]
	delete(handles, id)
	handlesMu.Unlock()
	if e != nil {
		e.mu.Lock()
		cache := e.cache
		e.cache = nil
		e.mu.Unlock()
		for _, o := range cache {
			pyDecRef(o)
		}
	}
	tp := pyType(self)
	free := handleFree.Load()
	if free == 0 || handleFreeMixed.Load() {
		free = pyTypeGetSlot(tp, slotTpFree)
	}
	callC(free, self)
	pyDecRef(tp)
	return 0
}
