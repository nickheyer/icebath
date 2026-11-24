package cpythonutil

import (
	"strings"
	"sync"
)

// Interned attribute and module name objects held for the process lifetime, so a lookup by name neither copies the name into C memory nor builds a str
var (
	namesMu sync.RWMutex
	names   = map[string]uintptr{}
)

// Names the cache holds at most, beyond which a lookup builds and drops its own name object
const maxNames = 1 << 12

// Name object for s, borrowed from the cache, or a new reference the caller drops when temp is true
func (g *GIL) name(s string) (o uintptr, temp bool, err error) {
	namesMu.RLock()
	o, ok := names[s]
	namesMu.RUnlock()
	if ok {
		return o, false, nil
	}
	if o, err = g.intern(s); err != nil {
		return 0, false, err
	}
	namesMu.Lock()
	prev, ok := names[s]
	full := !ok && len(names) >= maxNames
	if !ok && !full {
		names[strings.Clone(s)] = o
	}
	namesMu.Unlock()
	switch {
	case ok:
		pyDecRef(o)
		return prev, false, nil
	case full:
		return o, true, nil
	}
	return o, false, nil
}

// Interned str for s, a new reference
func (g *GIL) intern(s string) (uintptr, error) {
	o, err := g.newUnicode(s)
	if err != nil {
		return 0, err
	}
	pyUnicodeInternInPlace(&o)
	return o, nil
}

// Imports a module, a new reference
func (g *GIL) Import(name string) (uintptr, error) {
	n, temp, err := g.name(name)
	if err != nil {
		return 0, err
	}
	m := pyImportImport(n)
	if temp {
		pyDecRef(n)
	}
	if m == 0 {
		return 0, g.fetchError()
	}
	return m, nil
}

// Attribute lookup, a new reference
func (g *GIL) Attr(o uintptr, name string) (uintptr, error) {
	n, temp, err := g.name(name)
	if err != nil {
		return 0, err
	}
	v := pyObjectGetAttr(o, n)
	if temp {
		pyDecRef(n)
	}
	if v == 0 {
		return 0, g.fetchError()
	}
	return v, nil
}

// New reference to an attribute, 0 with the lookup failure cleared when there is none
func (g *GIL) optionalAttr(o uintptr, name string) uintptr {
	n, temp, err := g.name(name)
	if err != nil {
		return 0
	}
	v := pyObjectGetAttr(o, n)
	if temp {
		pyDecRef(n)
	}
	if v == 0 {
		pyErrClear()
	}
	return v
}

// Whether the object has the attribute, any lookup error counting as absent and from 3.13 handed to sys.unraisablehook by CPython
func (g *GIL) HasAttr(o uintptr, name string) bool {
	n, temp, err := g.name(name)
	if err != nil {
		return false
	}
	found := pyObjectHasAttr(o, n) == 1
	if temp {
		pyDecRef(n)
	}
	return found
}

// Sets an attribute from a Go value
func (g *GIL) SetAttr(o uintptr, name string, value any) error {
	v, err := g.ToPython(value)
	if err != nil {
		return err
	}
	defer pyDecRef(v)
	n, temp, err := g.name(name)
	if err != nil {
		return err
	}
	if temp {
		defer pyDecRef(n)
	}
	if pyObjectSetAttr(o, n, v) != 0 {
		return g.fetchError()
	}
	return nil
}
