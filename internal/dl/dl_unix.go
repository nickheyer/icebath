//go:build (darwin || freebsd || linux || netbsd) && !386 && !arm

package dl

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/ebitengine/purego"
)

// Whether this build can open shared libraries
const Supported = true

// Opens a shared library by loader name or path with every symbol resolved before the call returns
func Open(path string, scope Scope) (*Library, error) {
	mode := purego.RTLD_NOW | purego.RTLD_LOCAL
	if scope == Global {
		mode = purego.RTLD_NOW | purego.RTLD_GLOBAL
	}
	handle, err := purego.Dlopen(path, mode)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Library{path: path, handle: handle}, nil
}

// The running process image with every library already loaded into it
func Self() *Library { return &Library{path: "process", handle: purego.RTLD_DEFAULT} }

// Calls a C function by address with integer and pointer arguments, returning its result and the errno it set

//go:uintptrescapes
func Call(fn uintptr, args ...uintptr) (r1, errno uintptr) {
	return callWithErrno(fn, args)
}

// Calls a C variadic function whose first fixed arguments are named, the rest passed as variadic integers or pointers

//go:uintptrescapes
func CallVariadic(fn uintptr, fixed int, args ...uintptr) (r1, errno uintptr) {
	return callWithErrno(fn, variadicFrame(fixed, args))
}

// Result of a C call and the errno of its locked thread, zeroed before the call
func callWithErrno(fn uintptr, args []uintptr) (r1, errno uintptr) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	slot, _, _ := purego.SyscallN(errnoLocation())
	*At[int32](slot) = 0
	r1, _, _ = purego.SyscallN(fn, args...)
	return r1, uintptr(*At[int32](slot))
}

// Address of the libc function that yields the errno slot of the calling thread
var errnoLocation = sync.OnceValue(func() uintptr {
	name := "__errno_location"
	switch runtime.GOOS {
	case "darwin", "ios", "freebsd":
		name = "__error"
	case "netbsd":
		name = "__errno"
	}
	addr, err := purego.Dlsym(purego.RTLD_DEFAULT, name)
	if err != nil || addr == 0 {
		panic(fmt.Sprintf("dl: the C library of this process exports no %s", name))
	}
	return addr
})

// Arguments laid out for a variadic call, Apple arm64 taking every variadic one from the stack past eight registers
func variadicFrame(fixed int, args []uintptr) []uintptr {
	apple := runtime.GOOS == "darwin" || runtime.GOOS == "ios"
	if !apple || runtime.GOARCH != "arm64" || fixed >= 8 || fixed >= len(args) {
		return args
	}
	frame := make([]uintptr, 8, 8+len(args)-fixed)
	copy(frame, args[:fixed])
	return append(frame, args[fixed:]...)
}

// C function pointer that runs a Go function, allocated for the life of the process
func Callback(fn any) (uintptr, error) {
	checkCallback(fn)
	return purego.NewCallback(fn), nil
}

func lookup(handle uintptr, name string) (uintptr, error) { return purego.Dlsym(handle, name) }

func register(fn any, addr uintptr) { purego.RegisterFunc(fn, addr) }
