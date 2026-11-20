package provider

import "unsafe"

// Bytes of the NUL terminated C string at the address a native call returned
func cStringAt(p uintptr) []byte {
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	var out []byte
	for ; *(*byte)(ptr) != 0; ptr = unsafe.Add(ptr, 1) {
		out = append(out, *(*byte)(ptr))
	}
	return out
}
