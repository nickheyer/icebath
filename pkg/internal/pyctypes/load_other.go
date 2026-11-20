//go:build !((darwin || freebsd || linux || netbsd || windows) && !386 && !arm && !faketime)

package pyctypes

import "runtime"

// Whether this build can load shared libraries
const Supported = false

// Shared libraries cannot be loaded without a purego dynamic loader binding for this platform
func LoadLibrary(path string, global bool) (*CDLL, error) {
	return nil, &OSError{Msg: path + ": no dynamic loader binding on " + runtime.GOOS + "/" + runtime.GOARCH}
}

// No library is ever loaded on this platform, every lookup fails
func (l *CDLL) Symbol(name string) (uintptr, error) {
	return 0, &AttributeError{Msg: l.Name + ": undefined symbol: " + name}
}

// No C function can be bound on this platform
func RegisterFunc(fptr any, addr uintptr) {
	panic("pyctypes: no dynamic loader binding on " + runtime.GOOS + "/" + runtime.GOARCH)
}

// The DLL search path exists only on Windows, elsewhere nothing is added and the close does nothing
func AddDLLDirectory(path string) (func(), error) { return func() {}, nil }
