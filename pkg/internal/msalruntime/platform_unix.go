//go:build (darwin || linux) && (amd64 || arm64) && !faketime

package msalruntime

import "github.com/ebitengine/purego"

// Whether this build can load the native library
const loadSupported = true

// Opens the library the way pymsalruntime preloads it, RTLD_NOW with local symbol scope
func openLibrary(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
}

func lookupSymbol(lib uintptr, name string) (uintptr, error) { return purego.Dlsym(lib, name) }

// Calls a C function with pointer sized arguments and returns its first result register
//
//go:uintptrescapes
func call(fn uintptr, args ...uintptr) uintptr {
	r1, _, _ := purego.SyscallN(fn, args...)
	return r1
}

// C function pointer running fn for a completion routine taking a result handle and callbackData
func newCompletionCallback(fn func(result, data uintptr)) uintptr {
	return purego.NewCallback(func(result, data uintptr) uintptr {
		fn(result, data)
		return 0
	})
}

// C function pointer running fn for the log routine taking a message, a level and callbackData
func newLogCallback(fn func(message uintptr, level int32, data uintptr)) uintptr {
	return purego.NewCallback(func(message uintptr, level int32, data uintptr) uintptr {
		fn(message, level, data)
		return 0
	})
}
