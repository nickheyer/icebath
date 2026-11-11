package pyctypes

import "unsafe"

// The error ctypes raises when a shared library cannot be loaded
type OSError struct{ Msg string }

func (e *OSError) Error() string { return e.Msg }

// The error ctypes raises when a loaded library lacks a symbol
type AttributeError struct{ Msg string }

func (e *AttributeError) Error() string { return e.Msg }

// One loaded shared library
type CDLL struct {
	Name   string
	handle uintptr
}

// Bytes of the NUL terminated C string at an address, empty for a null pointer
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

// NUL terminated copy of a string for a char pointer argument
func CString(s string) []byte {
	out := make([]byte, len(s)+1)
	copy(out, s)
	return out
}

// Pointer sized value at index i of a C array of pointers
func PointerAt(array uintptr, i int) uintptr {
	base := *(*unsafe.Pointer)(unsafe.Pointer(&array))
	return *(*uintptr)(unsafe.Add(base, uintptr(i)*unsafe.Sizeof(uintptr(0))))
}
