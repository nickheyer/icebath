package main

import "unsafe"

// Address of the first byte of a NUL terminated buffer for a raw syscall argument
func unsafePointer(b []byte) unsafe.Pointer { return unsafe.Pointer(&b[0]) }
