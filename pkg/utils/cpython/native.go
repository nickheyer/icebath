//go:build ((darwin && (amd64 || arm64)) || (linux && (cgo || !android) && (amd64 || arm64 || loong64 || ppc64le || riscv64 || (cgo && s390x))) || (windows && (amd64 || arm64))) && !faketime

package cpythonutil

import "github.com/ebitengine/purego"

// Whether this build can call libpython and take its callbacks: purego has a call trampoline, real callbacks and a PyStatus path here
const supported = true

// Binds the C function at addr to the Go function variable fptr points to
func registerFunc(fptr any, addr uintptr) { purego.RegisterFunc(fptr, addr) }

// C function pointer that runs the Go function fn
func newCallback(fn any) uintptr { return purego.NewCallback(fn) }

// Calls the C function fn with its one pointer argument
func callC(fn, arg uintptr) { purego.SyscallN(fn, arg) }
