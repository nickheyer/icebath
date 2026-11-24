package cpythonutil

// Calls callable with Go arguments, a new reference
func (g *GIL) Call(callable uintptr, args []any, kwargs map[string]any) (uintptr, error) {
	t, err := g.tuple(args)
	if err != nil {
		return 0, err
	}
	var kw uintptr
	if len(kwargs) > 0 {
		if kw, err = dictOf(g, kwargs, 0); err != nil {
			pyDecRef(t)
			return 0, err
		}
	}
	r := pyObjectCall(callable, t, kw)
	if r == 0 {
		err = g.fetchError()
	}
	pyDecRef(t)
	if kw != 0 {
		pyDecRef(kw)
	}
	return r, err
}

// Calls a method by name, a new reference
func (g *GIL) CallMethod(o uintptr, name string, args []any, kwargs map[string]any) (uintptr, error) {
	m, err := g.Attr(o, name)
	if err != nil {
		return 0, err
	}
	r, err := g.Call(m, args, kwargs)
	pyDecRef(m)
	return r, err
}

// Calls fn with arg as its one positional argument without converting anything, a new reference
func (g *GIL) call1(fn, arg uintptr) (uintptr, error) {
	t := pyTupleNew(1)
	if t == 0 {
		return 0, g.fetchError()
	}
	pyIncRef(arg)
	pyTupleSetItem(t, 0, arg)
	r := pyObjectCall(fn, t, 0)
	var err error
	if r == 0 {
		err = g.fetchError()
	}
	pyDecRef(t)
	return r, err
}

// Decrements a reference obtained from this package
func (g *GIL) Decref(o uintptr) { pyDecRef(o) }

// Truth value of an object
func (g *GIL) Truthy(o uintptr) (bool, error) {
	switch pyObjectIsTrue(o) {
	case 1:
		return true, nil
	case 0:
		return false, nil
	}
	return false, g.fetchError()
}

// Text of str(o)
func (g *GIL) Str(o uintptr) (string, error) {
	s := pyObjectStr(o)
	if s == 0 {
		return "", g.fetchError()
	}
	defer pyDecRef(s)
	return g.unicode(s)
}

// Text of repr(o)
func (g *GIL) Repr(o uintptr) (string, error) {
	s := pyObjectRepr(o)
	if s == 0 {
		return "", g.fetchError()
	}
	defer pyDecRef(s)
	return g.unicode(s)
}

// Whether the object is callable
func (g *GIL) Callable(o uintptr) bool { return pyCallableCheck(o) == 1 }

// Whether o is an instance of cls, with the exception of a failing check as the error
func (g *GIL) IsInstance(o, cls uintptr) (bool, error) {
	switch pyObjectIsInstance(o, cls) {
	case 1:
		return true, nil
	case 0:
		return false, nil
	}
	return false, g.fetchError()
}

// Whether class o derives from cls, with the exception of a failing check as the error
func (g *GIL) IsSubclass(o, cls uintptr) (bool, error) {
	switch pyObjectIsSubclass(o, cls) {
	case 1:
		return true, nil
	case 0:
		return false, nil
	}
	return false, g.fetchError()
}

// Whether inspect.isawaitable reports the object awaitable, ErrNoLoop once the loop thread is gone
func (g *GIL) IsAwaitable(o uintptr) (bool, error) {
	if g.h.isAwaitable == 0 {
		return false, ErrNoLoop
	}
	r, err := g.call1(g.h.isAwaitable, o)
	if err != nil {
		return false, err
	}
	yes := r == pyTrue
	pyDecRef(r)
	return yes, nil
}
