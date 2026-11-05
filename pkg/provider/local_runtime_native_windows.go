package provider

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type nativeLibrary struct{ handle windows.Handle }

// Loads a DLL searching the default and added directories plus its own, the global flag has no Windows meaning
func openNativeLibrary(path string, global bool) (*nativeLibrary, error) {
	_ = global
	handle, err := windows.LoadLibraryEx(path, 0,
		windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS|windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR)
	if err != nil {
		return nil, err
	}
	return &nativeLibrary{handle: handle}, nil
}

func (l *nativeLibrary) sym(name string) (uintptr, error) {
	return windows.GetProcAddress(l.handle, name)
}

// Adds directory to the DLL search path until the returned release runs
func addNativeDLLDirectory(directory string) (func(), error) {
	path, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return nil, err
	}
	cookie, err := windows.AddDllDirectory(path)
	if err != nil {
		return nil, err
	}
	return func() { _ = windows.RemoveDllDirectory(cookie) }, nil
}

func callNative(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(fn, args...)
	return r
}

// Calls fn with a handle and two size_t out parameters
func callNativeOut(fn, handle uintptr, a, b *uint64) {
	syscall.SyscallN(fn, handle, uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)))
}

// Calls fn with an int out parameter followed by plain arguments and returns the int32 result
func callNativeIntOut(fn uintptr, out *int32, args ...uintptr) int32 {
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(unsafe.Pointer(out))}, args...)...)
	return int32(r)
}

// Calls fn with a size_t out parameter followed by plain arguments and returns the int32 result
func callNativeSizeOut(fn uintptr, out *uint64, args ...uintptr) int32 {
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(unsafe.Pointer(out))}, args...)...)
	return int32(r)
}
