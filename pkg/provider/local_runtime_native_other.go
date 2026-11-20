//go:build !windows && !((darwin || freebsd || linux || netbsd) && !android && !386 && !arm && !faketime)

package provider

import "fmt"

type nativeLibrary struct{}

// Shared libraries cannot be opened without a dynamic loader binding on this platform
func openNativeLibrary(path string, global bool) (*nativeLibrary, error) {
	return nil, fmt.Errorf("%s: dynamic loading is unavailable on this platform (global=%t)", path, global)
}

func (l *nativeLibrary) sym(name string) (uintptr, error) {
	return 0, fmt.Errorf("%s: no library", name)
}

func addNativeDLLDirectory(string) (func(), error) { return func() {}, nil }

func callNative(uintptr, ...uintptr) uintptr { return 0 }

func callNativeOut(uintptr, uintptr, *uint64, *uint64) {}

func callNativeIntOut(uintptr, *int32, ...uintptr) int32 { return -1 }

func callNativeSizeOut(uintptr, *uint64, ...uintptr) int32 { return -1 }
