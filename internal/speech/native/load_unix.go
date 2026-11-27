//go:build (((darwin || freebsd || netbsd) && (amd64 || arm64)) || (linux && !386 && !arm && (cgo || (!android && (amd64 || arm64 || loong64 || ppc64le || riscv64))))) && !faketime

package native

import "github.com/ebitengine/purego"

// Whether this build can load shared libraries
const Supported = true

// Library opened with every symbol bound at once, its symbols visible to libraries loaded later when global
func Open(name string, global bool) (*Library, error) {
	if name == "" {
		return nil, &LoadError{Err: ErrNotFound}
	}
	mode := purego.RTLD_NOW | purego.RTLD_LOCAL
	if global {
		mode = purego.RTLD_NOW | purego.RTLD_GLOBAL
	}
	handle, err := purego.Dlopen(name, mode)
	if err != nil {
		return nil, &LoadError{Name: name, Err: err}
	}
	return &Library{name: name, handle: handle}, nil
}

func (l *Library) Symbol(name string) (uintptr, error) {
	if l == nil || l.handle == 0 {
		return 0, &SymbolError{Library: l.label(), Symbol: name, Err: errNotOpen}
	}
	addr, err := purego.Dlsym(l.handle, name)
	if err != nil {
		return 0, &SymbolError{Library: l.name, Symbol: name, Err: err}
	}
	return addr, nil
}

// Address of a symbol in any library already loaded into the process
func Lookup(name string) (uintptr, error) {
	addr, err := purego.Dlsym(purego.RTLD_DEFAULT, name)
	if err != nil {
		return 0, &SymbolError{Symbol: name, Err: err}
	}
	return addr, nil
}

func RegisterFunc(fptr any, addr uintptr) { purego.RegisterFunc(fptr, addr) }
