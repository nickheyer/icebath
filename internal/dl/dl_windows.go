//go:build windows && !386 && !arm

package dl

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/windows"
)

// Whether this build can open shared libraries
const Supported = true

// Opens a DLL by name or path, resolving the dependencies of a path from its own directory as well
func Open(path string, _ Scope) (*Library, error) {
	flags := uintptr(windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS)
	name := path
	if strings.ContainsAny(name, `/\`) {
		full, err := filepath.Abs(name)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		name = full
		flags |= windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR
	}
	handle, err := windows.LoadLibraryEx(name, 0, flags)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	return &Library{path: name, handle: uintptr(handle)}, nil
}

// Calls a C function by address with integer and pointer arguments, returning its result and the last Windows error

//go:uintptrescapes
func Call(fn uintptr, args ...uintptr) (r1, errno uintptr) {
	r1, _, errno = purego.SyscallN(fn, args...)
	return r1, errno
}

// Calls a C variadic function, which Windows passes exactly like one with fixed integer and pointer arguments

//go:uintptrescapes
func CallVariadic(fn uintptr, _ int, args ...uintptr) (r1, errno uintptr) {
	r1, _, errno = purego.SyscallN(fn, args...)
	return r1, errno
}

// C function pointer that runs a Go function, allocated for the life of the process
func Callback(fn any) (uintptr, error) {
	checkCallback(fn)
	return purego.NewCallback(fn), nil
}

func lookup(handle uintptr, name string) (uintptr, error) {
	return windows.GetProcAddress(windows.Handle(handle), name)
}

func register(fn any, addr uintptr) { purego.RegisterFunc(fn, addr) }
