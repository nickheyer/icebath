package pyhost

import "context"

// Kind classifies an object by the builtin type flags of its class, subclasses included
type Kind int

const (
	KindOther Kind = iota
	KindNone
	KindBool
	KindInt
	KindFloat
	KindStr
	KindBytes
	KindByteArray
	KindDict
	KindList
	KindTuple
)

// Builtin kind of an object, KindOther for every class outside the builtin scalars and containers
func (g *GIL) KindOf(o uintptr) Kind {
	if o == pyNone {
		return KindNone
	}
	t := pyType(o)
	if t == pyBoolType {
		return KindBool
	}
	flags := pyTypeGetFlags(t)
	switch {
	case flags&tpflagsLong != 0:
		return KindInt
	case flags&tpflagsUnicode != 0:
		return KindStr
	case flags&tpflagsBytes != 0:
		return KindBytes
	case flags&tpflagsDict != 0:
		return KindDict
	case flags&tpflagsList != 0:
		return KindList
	case flags&tpflagsTuple != 0:
		return KindTuple
	}
	if t == pyFloatType || pyObjectIsInstance(o, pyFloatType) == 1 {
		return KindFloat
	}
	if t == pyByteArrayType {
		return KindByteArray
	}
	return KindOther
}

// Takes ownership of a new reference so Go releases it when the Ref is collected
func (g *GIL) Own(o uintptr) *Ref { return g.adopt(o) }

// Python error that re-raises the given exception instance when it reaches Python again
func (g *GIL) Raising(exc *Ref) error { return g.describe(g.NewRef(exc.obj)) }

// Property is a read only attribute computed by Go from the borrowed instance pointer
type Property func(g *GIL, self uintptr) (any, error)

// New class built by type(name, bases, namespace) whose RawFunc and Property members run Go code
func (g *GIL) NewClass(module, name string, bases []any, members map[string]any) (uintptr, error) {
	functools, err := g.Import("functools")
	if err != nil {
		return 0, err
	}
	defer pyDecRef(functools)
	builtins, err := g.Import("builtins")
	if err != nil {
		return 0, err
	}
	defer pyDecRef(builtins)
	ns := map[string]any{"__module__": module, "__qualname__": name}
	var held []uintptr
	defer func() {
		for _, o := range held {
			pyDecRef(o)
		}
	}()
	for key, member := range members {
		switch m := member.(type) {
		case RawFunc:
			fn, err := g.NewRawFunc(key, m)
			if err != nil {
				return 0, err
			}
			held = append(held, fn)
			bound, err := g.CallMethod(functools, "partialmethod", []any{Borrowed(fn)}, nil)
			if err != nil {
				return 0, err
			}
			held = append(held, bound)
			ns[key] = Borrowed(bound)
		case Property:
			fn, err := g.methodFunc(key, func(g *GIL, self uintptr, _ []any, _ map[string]any) (any, error) { return m(g, self) })
			if err != nil {
				return 0, err
			}
			held = append(held, fn)
			prop, err := g.CallMethod(builtins, "property", []any{Borrowed(fn)}, nil)
			if err != nil {
				return 0, err
			}
			held = append(held, prop)
			ns[key] = Borrowed(prop)
		default:
			ns[key] = member
		}
	}
	baseTuple, err := g.CallMethod(builtins, "tuple", []any{bases}, nil)
	if err != nil {
		return 0, err
	}
	defer pyDecRef(baseTuple)
	return g.CallMethod(builtins, "type", []any{name, Borrowed(baseTuple), ns}, nil)
}

// Native function receiving self as its first positional argument and the rest converted to Go
func (g *GIL) methodFunc(name string, m func(g *GIL, self uintptr, args []any, kwargs map[string]any) (any, error)) (uintptr, error) {
	return g.NewRawFunc(name, func(g *GIL, args, kwargs uintptr) (uintptr, error) {
		n := pyTupleSize(args)
		if n == 0 {
			return 0, &Exception{Type: "TypeError", Message: name + "() missing self"}
		}
		self := pyTupleGetItem(args, 0)
		rest := make([]any, 0, n-1)
		for i := 1; i < n; i++ {
			v, err := g.FromPython(pyTupleGetItem(args, i))
			if err != nil {
				return 0, err
			}
			rest = append(rest, v)
		}
		var kw map[string]any
		if kwargs != 0 {
			v, err := g.FromPython(kwargs)
			if err != nil {
				return 0, err
			}
			kw = v.(map[string]any)
		}
		out, err := m(g, self, rest, kw)
		if err != nil {
			return 0, err
		}
		return g.ToPython(out)
	})
}

// Runs the referenced awaitable as a task created inside the contextvars context pyctx and returns its raw result
func (r *Ref) AwaitIn(ctx context.Context, pyctx *Ref) (*Ref, error) {
	return r.h.awaitRaw(ctx, pyctx, func(g *GIL) (uintptr, error) {
		pyIncRef(r.obj)
		return r.obj, nil
	})
}

// Calls fn with each item of the iterable as a borrowed pointer until the iterator ends or fn fails
func (g *GIL) Iterate(o uintptr, fn func(item uintptr) error) error {
	it := pyObjectGetIter(o)
	if it == 0 {
		return g.fetchError()
	}
	defer pyDecRef(it)
	for {
		item := pyIterNext(it)
		if item == 0 {
			if pyErrOccurred() != 0 {
				return g.fetchError()
			}
			return nil
		}
		err := fn(item)
		pyDecRef(item)
		if err != nil {
			return err
		}
	}
}

// Whether the carried exception is an instance of any named builtin or module.Class exception class
func (e *PyError) Matches(g *GIL, names ...string) bool {
	if e.exc == nil {
		return false
	}
	for _, name := range names {
		cls, err := g.exceptionClass(name)
		if err == nil && pyObjectIsInstance(e.exc.obj, cls) == 1 {
			return true
		}
		pyErrClear()
	}
	return false
}

// Exception instance the error carries, nil for an error raised from Go
func (e *PyError) Exception() *Ref { return e.exc }

// Borrowed event loop running on the host loop thread
func (g *GIL) Loop() uintptr { return g.h.loop }

// Drops every reference Go released since the last GIL entry, while the GIL is held
func (g *GIL) Drain() { g.h.drainReleases() }
