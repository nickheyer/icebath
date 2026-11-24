//go:build ((darwin && (amd64 || arm64)) || (linux && (cgo || !android) && (amd64 || arm64 || loong64 || ppc64le || riscv64 || (cgo && s390x)))) && !faketime

package cpythonutil

import (
	"encoding/binary"
	"unicode/utf8"

	"github.com/ebitengine/purego"
)

// Opens libpython globally so extension modules resolve the C API from it
func openLibrary(path string) (func(string) (uintptr, error), error) {
	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	return func(name string) (uintptr, error) { return purego.Dlsym(handle, name) }, nil
}

// Encodes s as a NUL terminated wchar_t buffer in Go memory, wchar_t being UTF-32 in the machine's byte order on Linux and macOS
func wideString(s string) []byte {
	out := make([]byte, 0, (utf8.RuneCountInString(s)+1)*4)
	for _, r := range s {
		out = binary.NativeEndian.AppendUint32(out, uint32(r))
	}
	return append(out, 0, 0, 0, 0)
}
