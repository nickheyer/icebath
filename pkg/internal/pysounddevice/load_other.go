//go:build !((darwin || freebsd || linux || netbsd || windows) && !386 && !arm && !faketime)

package pysounddevice

import (
	"runtime"
	"unsafe"
)

type culong = uint64
type clong = int64

// PortAudio cannot be loaded without a purego dynamic loader binding for this platform
func openPortAudio() error {
	return &UnsupportedError{Msg: "This icebath build for " + runtime.GOOS + "/" + runtime.GOARCH + " has no dynamic library loader, so it cannot load PortAudio"}
}

func ignoreStderr() func() { return func() {} }

func newStreamCallback(func(input, output unsafe.Pointer, frameCount uintptr, timeInfo unsafe.Pointer, statusFlags, userData uintptr) uintptr) uintptr {
	return 0
}
