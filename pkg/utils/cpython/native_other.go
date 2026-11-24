//go:build !(((darwin && (amd64 || arm64)) || (linux && (cgo || !android) && (amd64 || arm64 || loong64 || ppc64le || riscv64 || (cgo && s390x))) || (windows && (amd64 || arm64))) && !faketime)

package cpythonutil

import "unsafe"

// Whether this build can call libpython and take its callbacks: purego cannot do both here, so Start reports ErrUnsupported
const supported = false

// Never opens anything, the platform has no binding to call libpython through
func openLibrary(string) (func(string) (uintptr, error), error) { return nil, ErrUnsupported }

// Unreachable: Start fails with ErrUnsupported before any symbol is bound, so these only keep the platform independent code compiling
func registerFunc(any, uintptr) { panic(unreachable) }

func newCallback(any) uintptr { panic(unreachable) }

func callC(uintptr, uintptr) { panic(unreachable) }

func wideString(string) []byte { panic(unreachable) }

func bindStatus(func(string) (uintptr, error)) error { return ErrUnsupported }

func statusPreInitialize(uintptr) pyStatus { panic(unreachable) }

func statusConfigSetString(uintptr, uintptr, unsafe.Pointer) pyStatus { panic(unreachable) }

func statusConfigSetArgv(uintptr, int, unsafe.Pointer) pyStatus { panic(unreachable) }

func statusInitializeFromConfig(uintptr) pyStatus { panic(unreachable) }

const unreachable = "cpython: C call on a platform without embedded CPython support"
