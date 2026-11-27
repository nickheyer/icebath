//go:build !((((darwin || freebsd || netbsd) && (amd64 || arm64)) || (linux && !386 && !arm && (cgo || (!android && (amd64 || arm64 || loong64 || ppc64le || riscv64)))) || (windows && (amd64 || arm64))) && !faketime)

package native

// Whether this build can load shared libraries
const Supported = false

func Open(name string, global bool) (*Library, error) {
	return nil, &LoadError{Name: name, Err: ErrUnsupported}
}

func (l *Library) Symbol(name string) (uintptr, error) {
	return 0, &SymbolError{Library: l.label(), Symbol: name, Err: ErrUnsupported}
}

func Lookup(name string) (uintptr, error) {
	return 0, &SymbolError{Symbol: name, Err: ErrUnsupported}
}

// Never reached: no symbol address is ever handed out to bind on this platform
func RegisterFunc(fptr any, addr uintptr) { panic("native: " + ErrUnsupported.Error()) }
