//go:build windows

package gateway

import "unsafe"

// Address of a UTF-16 string for a syscall argument
func uintptrOf(p *uint16) uintptr { return uintptr(unsafe.Pointer(p)) }
