//go:build !((((darwin || freebsd || netbsd) && (amd64 || arm64)) || (linux && !386 && !arm && (cgo || (!android && (amd64 || arm64 || loong64 || ppc64le || riscv64)))) || (windows && (amd64 || arm64))) && !faketime)

package dlutil

// Whether this build can load shared libraries
const Supported = false

// Always a LoadError wrapping ErrUnsupported, this platform has no loader binding
func Open(name string, global bool) (*Library, error) {
	return nil, &LoadError{Name: name, Err: ErrUnsupported}
}

// Always a SymbolError wrapping ErrUnsupported, no library is ever open on this platform
func (l *Library) Symbol(name string) (uintptr, error) {
	return 0, &SymbolError{Library: l.label(), Symbol: name, Err: ErrUnsupported}
}

// Always a SymbolError wrapping ErrUnsupported
func Lookup(name string) (uintptr, error) {
	return 0, &SymbolError{Symbol: name, Err: ErrUnsupported}
}

// Panics, no C function can be bound on this platform and no symbol address is ever handed out to bind
func RegisterFunc(fptr any, addr uintptr) { panic("dlutil: " + ErrUnsupported.Error()) }
