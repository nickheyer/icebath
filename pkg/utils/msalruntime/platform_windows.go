//go:build windows && (amd64 || arm64 || 386)

package msalruntime

import (
	"fmt"
	"path/filepath"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/windows"
)

// Whether this build can load the native library
const loadSupported = true

// Loads the DLL with its own directory searched for the libraries it depends on
func openLibrary(path string) (uintptr, error) {
	full, err := filepath.Abs(path)
	if err != nil {
		full = path
	}
	h, err := windows.LoadLibraryEx(full, 0, windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS|windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR)
	if err != nil {
		return 0, fmt.Errorf("Could not find module '%s' (or one of its dependencies): %v", full, err)
	}
	return uintptr(h), nil
}

func lookupSymbol(lib uintptr, name string) (uintptr, error) {
	addr, err := windows.GetProcAddress(windows.Handle(lib), name)
	if err != nil || addr == 0 {
		return 0, fmt.Errorf("function '%s' not found", name)
	}
	return addr, nil
}

// Calls a cdecl C function with pointer sized arguments and returns its result register
//
//go:uintptrescapes
func call(fn uintptr, args ...uintptr) uintptr {
	r1, _, _ := purego.SyscallN(fn, args...)
	return r1
}

// cdecl function pointer running fn for a completion routine taking a result handle and callbackData
func newCompletionCallback(fn func(result, data uintptr)) uintptr {
	return purego.NewCallback(func(_ purego.CDecl, result, data uintptr) uintptr {
		fn(result, data)
		return 0
	})
}

// cdecl function pointer running fn for the log routine taking a message, a level and callbackData
func newLogCallback(fn func(message uintptr, level int32, data uintptr)) uintptr {
	return purego.NewCallback(func(_ purego.CDecl, message uintptr, level int32, data uintptr) uintptr {
		fn(message, level, data)
		return 0
	})
}
