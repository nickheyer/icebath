package fetch

import (
	"fmt"
	"unsafe"
)

// A native library loaded from a fetched file
type Library struct {
	Path   string
	handle uintptr
}

// Binding of one Go function variable to a symbol of the library
type Binding struct {
	Fn   any
	Name string
}

// Binds every function variable to its symbol, naming the first symbol the library lacks
func (l *Library) BindAll(bindings []Binding) error {
	for _, b := range bindings {
		addr, err := l.Symbol(b.Name)
		if err != nil {
			return err
		}
		registerFunc(b.Fn, addr)
	}
	return nil
}

// Binds one function variable to a symbol
func (l *Library) Bind(fn any, name string) error {
	return l.BindAll([]Binding{{fn, name}})
}

// Address of an exported symbol
func (l *Library) Symbol(name string) (uintptr, error) {
	addr, err := symbol(l.handle, name)
	if err != nil || addr == 0 {
		return 0, fmt.Errorf("%s does not export %s", l.Path, name)
	}
	return addr, nil
}

// NUL terminated copy of a string for a char pointer argument
func CString(s string) *byte {
	out := make([]byte, len(s)+1)
	copy(out, s)
	return &out[0]
}

// Copy of the NUL terminated C string at an address, empty for a null pointer
func GoString(p uintptr) string {
	if p == 0 {
		return ""
	}
	base := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	n := 0
	for *(*byte)(unsafe.Add(base, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(base), n))
}

// Pointer sized value at index i of a C array of pointers
func PointerAt(array uintptr, i int) uintptr {
	base := *(*unsafe.Pointer)(unsafe.Pointer(&array))
	return *(*uintptr)(unsafe.Add(base, uintptr(i)*unsafe.Sizeof(uintptr(0))))
}

// Typed view of library memory at an address
func Deref[T any](addr uintptr) *T { return (*T)(*(*unsafe.Pointer)(unsafe.Pointer(&addr))) }

// Copy of n values of library memory starting at an address
func CopyOut[T any](addr uintptr, n int) []T {
	if addr == 0 || n <= 0 {
		return nil
	}
	return append([]T(nil), unsafe.Slice(Deref[T](addr), n)...)
}
