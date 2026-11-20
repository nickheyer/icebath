//go:build !windows

package msalruntime

import (
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
)

// NUL terminated os_char string, UTF-8 bytes outside Windows
type osString []byte

func encodeOSString(s string) osString {
	b := make([]byte, len(s)+1)
	copy(b, s)
	return b
}

func (o osString) ptr() uintptr { return uintptr(unsafe.Pointer(&o[0])) }

// Buffer of os_char units a string getter fills
type osBuffer []byte

func newOSBuffer(n int32) osBuffer { return make([]byte, n) }

func (b osBuffer) ptr() uintptr { return uintptr(unsafe.Pointer(&b[0])) }

// Text up to the first NUL
func (b osBuffer) text() string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// Text of a NUL terminated os_char string the runtime owns
func decodeOSString(p uintptr) string { return pyctypes.GoString(p) }
