//go:build (darwin || freebsd || linux || netbsd) && !android && !386 && !arm && !faketime

package provider

import (
	"unsafe"

	"github.com/ebitengine/purego"
)

type nativeLibrary struct{ handle uintptr }

// Loads a shared library with immediate binding, global symbol visibility when global is set
func openNativeLibrary(path string, global bool) (*nativeLibrary, error) {
	mode := purego.RTLD_NOW | purego.RTLD_LOCAL
	if global {
		mode = purego.RTLD_NOW | purego.RTLD_GLOBAL
	}
	handle, err := purego.Dlopen(path, mode)
	if err != nil {
		return nil, err
	}
	return &nativeLibrary{handle: handle}, nil
}

func (l *nativeLibrary) sym(name string) (uintptr, error) { return purego.Dlsym(l.handle, name) }

// The loader searches library directories itself, nothing to add or release
func addNativeDLLDirectory(string) (func(), error) { return func() {}, nil }

func callNative(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := purego.SyscallN(fn, args...)
	return r
}

// Calls fn with a handle and two size_t out parameters
func callNativeOut(fn, handle uintptr, a, b *uint64) {
	purego.SyscallN(fn, handle, uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)))
}

// Calls fn with an int out parameter followed by plain arguments and returns the int32 result
func callNativeIntOut(fn uintptr, out *int32, args ...uintptr) int32 {
	r, _, _ := purego.SyscallN(fn, append([]uintptr{uintptr(unsafe.Pointer(out))}, args...)...)
	return int32(r)
}

// Calls fn with a size_t out parameter followed by plain arguments and returns the int32 result
func callNativeSizeOut(fn uintptr, out *uint64, args ...uintptr) int32 {
	r, _, _ := purego.SyscallN(fn, append([]uintptr{uintptr(unsafe.Pointer(out))}, args...)...)
	return int32(r)
}
