package cpythonutil

import (
	"errors"
	"sync"
)

// Go function callable from Python with its arguments converted to Go values
type Func func(g *GIL, args []any, kwargs map[string]any) (any, error)

// Go function over the borrowed argument tuple and keyword dict, 0 for no keywords, that returns a new reference
type RawFunc func(g *GIL, args, kwargs uintptr) (uintptr, error)

// Func with the __name__ Python shows for it
type NamedFunc struct {
	Name string
	Fn   Func
}

// Native callable named name that runs fn, a new reference
func (g *GIL) NewFunc(name string, fn Func) (uintptr, error) {
	return g.NewRawFunc(name, func(g *GIL, args, kwargs uintptr) (uintptr, error) {
		a, err := g.tupleValue(args, 0)
		if err != nil {
			return 0, err
		}
		var kw map[string]any
		if kwargs != 0 {
			if kw, err = g.dictValue(kwargs, 0); err != nil {
				return 0, err
			}
		}
		out, err := fn(g, a, kw)
		if err != nil {
			return 0, err
		}
		return g.ToPython(out)
	})
}

// Native callable over raw pointers, a new reference whose self object carries the Go function
func (g *GIL) NewRawFunc(name string, fn RawFunc) (uintptr, error) { return g.NewDocFunc(name, "", fn) }

// Native callable named name with ml_doc set to doc, whose leading name(params) line becomes its text signature, a new reference
func (g *GIL) NewDocFunc(name, doc string, fn RawFunc) (uintptr, error) {
	def, err := methodDef(name, doc)
	if err != nil {
		return 0, err
	}
	self, err := g.newHandleObject("icebath", "GoFunctionHandle", &handleEntry{raw: fn})
	if err != nil {
		return 0, err
	}
	defer pyDecRef(self)
	f := pyCFunctionNewEx(def, self, 0)
	if f == 0 {
		return 0, g.fetchError()
	}
	return f, nil
}

// PyMethodDef records, one per name and doc text, shared by every callable built from them and never freed
var (
	defsMu     sync.Mutex
	methodDefs = map[[2]string]uintptr{}
)

// PyMethodDef for name and doc whose ml_meth is the shared method trampoline, ml_doc NULL for an empty doc
func methodDef(name, doc string) (uintptr, error) {
	defsMu.Lock()
	defer defsMu.Unlock()
	key := [2]string{name, doc}
	if def, ok := methodDefs[key]; ok {
		return def, nil
	}
	def := pyMemRawMalloc(32)
	cname := permanentCString(name)
	var cdoc uintptr
	if doc != "" {
		cdoc = permanentCString(doc)
	}
	if def == 0 || cname == 0 || (doc != "" && cdoc == 0) {
		return 0, errors.New("cpython: out of memory for a method definition")
	}
	*(*uintptr)(cptr(def)) = cname
	*(*uintptr)(cptr(def + 8)) = cbMethod
	*(*int32)(cptr(def + 16)) = methVarargsKeywords
	*(*int32)(cptr(def + 20)) = 0
	*(*uintptr)(cptr(def + 24)) = cdoc
	methodDefs[key] = def
	return def, nil
}
