//go:build windows

package msalruntime

import (
	"unicode/utf16"
	"unsafe"
)

// NUL terminated os_char string, UTF-16 wchar_t units on Windows
type osString []uint16

func encodeOSString(s string) osString { return append(utf16.Encode([]rune(s)), 0) }

func (o osString) ptr() uintptr { return uintptr(unsafe.Pointer(&o[0])) }

// Buffer of wchar_t units a string getter fills
type osBuffer []uint16

func newOSBuffer(n int32) osBuffer { return make([]uint16, n) }

func (b osBuffer) ptr() uintptr { return uintptr(unsafe.Pointer(&b[0])) }

// Text up to the first NUL unit
func (b osBuffer) text() string {
	for i, c := range b {
		if c == 0 {
			return string(utf16.Decode(b[:i]))
		}
	}
	return string(utf16.Decode(b))
}

// Text of a NUL terminated wchar_t string the runtime owns
func decodeOSString(p uintptr) string {
	if p == 0 {
		return ""
	}
	base := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	n := 0
	for *(*uint16)(unsafe.Add(base, 2*n)) != 0 {
		n++
	}
	return string(utf16.Decode(unsafe.Slice((*uint16)(base), n)))
}
