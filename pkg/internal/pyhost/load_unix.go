//go:build darwin || linux

package pyhost

import "github.com/ebitengine/purego"

// wchar_t is UTF-32 on Linux and macOS
const wcharSize = 4

// Opens libpython globally so extension modules resolve the C API from it
func openLibrary(path string) (func(string) (uintptr, error), error) {
	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	return func(name string) (uintptr, error) { return purego.Dlsym(handle, name) }, nil
}

// Encodes s as a NUL terminated wchar_t buffer in Go memory
func wideString(s string) []byte {
	runes := []rune(s)
	out := make([]byte, 0, (len(runes)+1)*4)
	for _, r := range runes {
		out = append(out, byte(r), byte(r>>8), byte(r>>16), byte(r>>24))
	}
	return append(out, 0, 0, 0, 0)
}
