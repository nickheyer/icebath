//go:build windows && (amd64 || arm64) && !faketime

package cpythonutil

import (
	"syscall"
	"unicode"
	"unicode/utf16"
	"unsafe"
)

var loadLibraryExW = syscall.NewLazyDLL("kernel32.dll").NewProc("LoadLibraryExW")

const loadWithAlteredSearchPath = 0x00000008

// Loads python3X.dll by full path so the vcruntime140.dll beside it resolves from the same directory
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

// Encodes s as a NUL terminated wchar_t buffer in Go memory, wchar_t being UTF-16 on Windows
func wideString(s string) []byte {
	out := make([]byte, 0, 2*len(s)+2)
	for _, r := range s {
		if r1, r2 := utf16.EncodeRune(r); r1 != unicode.ReplacementChar || r2 != unicode.ReplacementChar {
			out = append(out, byte(r1), byte(r1>>8), byte(r2), byte(r2>>8))
			continue
		}
		out = append(out, byte(r), byte(r>>8))
	}
	return append(out, 0, 0)
}
