package cpythonutil

import (
	"runtime"
	"sync"
)

// Existing object pointer passed as an argument, the callee taking its own reference
type Borrowed uintptr

// Owned reference held by Go, usable from any goroutine and released under the GIL
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

// Takes ownership of a new reference so Go releases it when the Ref is collected
func (g *GIL) Own(o uintptr) *Ref { return g.adopt(o) }

// Wraps a new reference without incrementing it, queueing its release should the Ref be collected unreleased
func (g *GIL) adopt(o uintptr) *Ref {
	r := &Ref{h: g.h, obj: o}
	h := g.h
	r.cleanup = runtime.AddCleanup(r, func(p uintptr) { h.queueRelease(p) }, o)
	return r
}

// Borrowed pointer of the referenced object, valid while the Ref is held, 0 for a nil Ref
func (r *Ref) Ptr() uintptr {
	if r == nil {
		return 0
	}
	return r.obj
}

// Drops Go's reference at the next GIL entry, once however often it is called, nothing for a nil Ref
func (r *Ref) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.cleanup.Stop()
		r.h.queueRelease(r.obj)
	})
}

// Calls the referenced callable from any goroutine and converts the result to Go
func (r *Ref) Call(args []any, kwargs map[string]any) (any, error) {
	var out any
	err := r.h.Do(func(g *GIL) error {
		var err error
		out, err = r.CallLocked(g, args, kwargs)
		return err
	})
	return out, err
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

// Reads an attribute of the referenced object from any goroutine
func (r *Ref) GetAttr(name string) (any, error) {
	var out any
	err := r.h.Do(func(g *GIL) error {
		var err error
		out, err = r.GetAttrLocked(g, name)
		return err
	})
	return out, err
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

// Qualified class name of the referenced object, empty once the host is shut down
func (r *Ref) TypeName() string {
	var out string
	_ = r.h.Do(func(g *GIL) error {
		out = g.qualifiedName(pyType(r.obj))
		return nil
	})
	return out
}
