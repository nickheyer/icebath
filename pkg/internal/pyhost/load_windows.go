//go:build windows

package pyhost

import (
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// wchar_t is UTF-16 on Windows
const wcharSize = 2

var loadLibraryExW = syscall.NewLazyDLL("kernel32.dll").NewProc("LoadLibraryExW")

const loadWithAlteredSearchPath = 0x00000008

// Loads python3X.dll by full path so vcruntime140.dll beside it resolves from the same directory
func openLibrary(path string) (func(string) (uintptr, error), error) {
	wpath, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, _, callErr := loadLibraryExW.Call(uintptr(unsafe.Pointer(wpath)), 0, loadWithAlteredSearchPath)
	if h == 0 {
		return nil, callErr
	}
	handle := syscall.Handle(h)
	return func(name string) (uintptr, error) { return syscall.GetProcAddress(handle, name) }, nil
}

// Encodes s as a NUL terminated wchar_t buffer in Go memory
func wideString(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, (len(units)+1)*2)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return append(out, 0, 0)
}
