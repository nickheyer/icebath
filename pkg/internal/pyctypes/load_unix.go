//go:build (darwin || freebsd || linux || netbsd) && !386 && !arm && !faketime

package pyctypes

import "github.com/ebitengine/purego"

// Whether this build can load shared libraries
const Supported = true

// Loads a shared library the way ctypes CDLL does, RTLD_NOW with local or global symbol scope
func LoadLibrary(path string, global bool) (*CDLL, error) {
	mode := purego.RTLD_NOW | purego.RTLD_LOCAL
	if global {
		mode = purego.RTLD_NOW | purego.RTLD_GLOBAL
	}
	handle, err := purego.Dlopen(path, mode)
	if err != nil {
		return nil, &OSError{Msg: err.Error()}
	}
	return &CDLL{Name: path, handle: handle}, nil
}

// Address of an exported symbol, an AttributeError carrying the loader message when absent
func (l *CDLL) Symbol(name string) (uintptr, error) {
	addr, err := purego.Dlsym(l.handle, name)
	if err != nil {
		return 0, &AttributeError{Msg: err.Error()}
	}
	return addr, nil
}

// Binds a Go function variable to a C function address
func RegisterFunc(fptr any, addr uintptr) { purego.RegisterFunc(fptr, addr) }

// The DLL search path exists only on Windows, elsewhere nothing is added and the close does nothing
func AddDLLDirectory(path string) (func(), error) { return func() {}, nil }
