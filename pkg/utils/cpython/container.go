package cpythonutil

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

// Borrowed sys.modules
func (g *GIL) SysModules() uintptr { return pySysGetObject("modules") }

// Stores a value in a dict under a string key
func (g *GIL) DictSet(d uintptr, key string, value any) error { return g.setItem(d, key, value, 0) }

// Keys of a dict as Go strings, a key that is not a str by its str() text
func (g *GIL) DictKeys(d uintptr) ([]string, error) {
	w, err := g.walkDict(d)
	if err != nil {
		return nil, err
	}
	defer w.close()
	var out []string
	for {
		k, _, ok := w.next()
		if !ok {
			return out, nil
		}
		var s string
		if isStr(k) {
			s, err = g.unicode(k)
		} else {
			// str() may run Python code that changes the dict, so the key is held across it
			pyIncRef(k)
			s, err = g.Str(k)
			pyDecRef(k)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
}

// Removes a string key from a dict, ignoring a missing key
func (g *GIL) DictDel(d uintptr, key string) {
	if pyDictDelItemString(d, key) != 0 {
		pyErrClear()
	}
}

// Borrowed item i of a tuple, 0 when i is out of range
func (g *GIL) TupleItem(t uintptr, i int) uintptr {
	o := pyTupleGetItem(t, i)
	if o == 0 {
		pyErrClear()
	}
	return o
}

// New reference to None
func (g *GIL) None() uintptr {
	pyIncRef(pyNone)
	return pyNone
}

// Whether the pointer is the None singleton
func (g *GIL) IsNone(o uintptr) bool { return o == pyNone }
