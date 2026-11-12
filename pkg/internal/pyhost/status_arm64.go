//go:build arm64 && (darwin || linux || windows)

package pyhost

import (
	"fmt"
	"runtime"
	"unsafe"
)

//go:linkname runtimeCgocall runtime.cgocall
func runtimeCgocall(fn uintptr, arg unsafe.Pointer) int32

// Entry of the assembly trampoline that loads the indirect result register X8 before the call
var x8callABI0 uintptr

// Frame the trampoline reads, the C function, the PyStatus buffer for X8 and four integer arguments
type x8frame struct {
	fn  uintptr
	ret uintptr
	a   [4]uintptr
}

var statusAddrs [4]uintptr

// Resolves the PyStatus returning entry points, called through one X8 trampoline on every arm64 system
func bindStatus(lookup func(string) (uintptr, error)) error {
	for i, name := range statusSymbolNames {
		addr, err := lookup(name)
		if err != nil || addr == 0 {
			return fmt.Errorf("libpython lacks %s: %v", name, err)
		}
		statusAddrs[i] = addr
	}
	return nil
}

// Calls fn on the system stack with the PyStatus buffer in X8 as AAPCS64 requires for a 32 byte result
func callStatus(fn uintptr, args ...uintptr) pyStatus {
	st := new(pyStatus)
	f := &x8frame{fn: fn, ret: uintptr(unsafe.Pointer(st))}
	copy(f.a[:], args)
	runtimeCgocall(x8callABI0, unsafe.Pointer(f))
	runtime.KeepAlive(f)
	runtime.KeepAlive(st)
	return *st
}

func statusPreInitialize(pre uintptr) pyStatus { return callStatus(statusAddrs[0], pre) }

func statusConfigSetString(cfg, field uintptr, s unsafe.Pointer) pyStatus {
	st := callStatus(statusAddrs[1], cfg, field, uintptr(s))
	runtime.KeepAlive(s)
	return st
}

func statusConfigSetArgv(cfg uintptr, argc int, argv unsafe.Pointer) pyStatus {
	st := callStatus(statusAddrs[2], cfg, uintptr(argc), uintptr(argv))
	runtime.KeepAlive(argv)
	return st
}

func statusInitializeFromConfig(cfg uintptr) pyStatus { return callStatus(statusAddrs[3], cfg) }
