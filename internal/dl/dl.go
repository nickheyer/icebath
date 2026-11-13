// Loads shared libraries, binds their symbols to Go functions and reads their memory without cgo
package dl

import (
	"errors"
	"fmt"
	"reflect"
	"unsafe"
)

// Reported when the running build has no dynamic loader binding
var ErrUnsupported = errors.New("no dynamic loader for this operating system and architecture")

// Reported when a library does not export a requested symbol
var ErrNoSymbol = errors.New("symbol not exported")

// Reported when no installed library answers to a short name
var ErrNotFound = errors.New("library not installed")

// Symbol visibility a library is opened with on systems that have a global namespace
type Scope int

const (
	// Symbols stay private to the opened library
	Local Scope = iota
	// Symbols resolve references of libraries opened afterwards
	Global
)

// One opened shared library
type Library struct {
	path   string
	handle uintptr
}

// One Go function variable paired with the symbol it is bound to
type Binding struct {
	Fn   any
	Name string
}

// Address of an exported function or variable
func (l *Library) Symbol(name string) (uintptr, error) {
	addr, err := lookup(l.handle, name)
	if errors.Is(err, ErrUnsupported) {
		return 0, fmt.Errorf("%s: %s: %w", l.path, name, ErrUnsupported)
	}
	if err != nil || addr == 0 {
		return 0, fmt.Errorf("%s: %s: %w", l.path, name, ErrNoSymbol)
	}
	return addr, nil
}

// Points one Go function variable at an exported C function
func (l *Library) Bind(fn any, name string) error {
	return l.BindAll([]Binding{{Fn: fn, Name: name}})
}

// Points every function variable at its symbol, leaving all of them untouched when any symbol is missing
func (l *Library) BindAll(bindings []Binding) error {
	addrs := make([]uintptr, len(bindings))
	for i, b := range bindings {
		checkBindable(b.Fn)
		addr, err := l.Symbol(b.Name)
		if err != nil {
			return err
		}
		addrs[i] = addr
	}
	for i, b := range bindings {
		register(b.Fn, addrs[i])
	}
	return nil
}

// Panics unless fn points at a func variable whose calls never mint callbacks or expand Go variadic arguments
func checkBindable(fn any) {
	t := reflect.TypeOf(fn)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Func {
		panic(fmt.Sprintf("dl: bind target %T is not a pointer to a func variable", fn))
	}
	ft := t.Elem()
	if ft.IsVariadic() {
		panic(fmt.Sprintf("dl: bind target %s is variadic, C variadic functions go through CallVariadic", ft))
	}
	for i := range ft.NumIn() {
		if ft.In(i).Kind() == reflect.Func {
			panic(fmt.Sprintf("dl: bind target %s takes a func, pass a pointer made once by Callback instead", ft))
		}
	}
}

// Panics unless fn takes machine word arguments and returns one pointer sized word, the shape every platform accepts
func checkCallback(fn any) {
	t := reflect.TypeOf(fn)
	ok := t != nil && t.Kind() == reflect.Func && !t.IsVariadic() && t.NumOut() == 1 &&
		machineWord(t.Out(0)) && t.Out(0).Size() == unsafe.Sizeof(uintptr(0))
	for i := 0; ok && i < t.NumIn(); i++ {
		ok = machineWord(t.In(i))
	}
	if !ok {
		panic(fmt.Sprintf("dl: callback %T needs integer or pointer arguments and one pointer sized integer or pointer result", fn))
	}
}

// Whether a type travels in one integer register
func machineWord(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Pointer, reflect.UnsafePointer:
		return true
	}
	return false
}

// NUL terminated copy of a string in Go memory for a char pointer argument or field
func CString(s string) *byte {
	out := make([]byte, len(s)+1)
	copy(out, s)
	return &out[0]
}

// Copy of the NUL terminated string at an address, empty for a null address
func GoString(addr uintptr) string {
	if addr == 0 {
		return ""
	}
	base := pointer(addr)
	n := 0
	for *(*byte)(unsafe.Add(base, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(base), n))
}

// Typed view of library owned memory at an address, nil for a null address
func At[T any](addr uintptr) *T { return (*T)(pointer(addr)) }

// Duplicate of n consecutive values of library owned memory, nil for a null address
func Copy[T any](addr uintptr, n int) []T {
	if addr == 0 || n <= 0 {
		return nil
	}
	return append([]T(nil), unsafe.Slice(At[T](addr), n)...)
}

// Reinterprets an address as memory the Go heap does not own
func pointer(addr uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&addr)) }
