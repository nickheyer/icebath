package dlutil

import "unsafe"

// NUL terminated copy of s for a char pointer argument, to be kept referenced for as long as C reads it
func CString(s string) []byte {
	out := make([]byte, len(s)+1)
	copy(out, s)
	return out
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
