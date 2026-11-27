// Native libraries speech runs on, loaded at run time without cgo: PortAudio, the speech engine and the wake word engines
package native

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"
)

// No dynamic library loader for this platform
var ErrUnsupported = errors.New("this build of icebath can't load libraries on " + runtime.GOOS + "/" + runtime.GOARCH)

// No library turned up for a name, path or search
var ErrNotFound = errors.New("the library was not found")

// No trampoline through which C can call a Go function in this build
var ErrNoCallback = errors.New("this build of icebath can't take calls from libraries on " + runtime.GOOS + "/" + runtime.GOARCH)

var errNotOpen = errors.New("the library is not open")

// A shared library that could not be opened, with the loader's reason
type LoadError struct {
	Name string
	Err  error
}

func (e *LoadError) Error() string { return subject(text(e.Err, "cannot be opened"), e.Name) }

func (e *LoadError) Unwrap() error { return e.Err }

// A function or variable a library does not export
type SymbolError struct {
	Library string
	Symbol  string
	Err     error
}

func (e *SymbolError) Error() string {
	message := text(e.Err, "not found")
	if e.Symbol != "" && !strings.Contains(message, e.Symbol) {
		message = "symbol " + e.Symbol + ": " + message
	}
	return subject(message, e.Library)
}

func (e *SymbolError) Unwrap() error { return e.Err }

// A callback the platform refused to make, most often past the number it allows
type callbackError struct{ reason any }

func (e *callbackError) Error() string { return fmt.Sprintf("new C callback: %v", e.reason) }

// One loaded shared library, kept loaded for the life of the process
type Library struct {
	name   string
	handle uintptr
}

func (l *Library) label() string {
	if l == nil {
		return ""
	}
	return l.name
}

// Binds a Go function variable to the library's function of the name
func (l *Library) Bind(fptr any, name string) error {
	addr, err := l.Symbol(name)
	if err != nil {
		return err
	}
	RegisterFunc(fptr, addr)
	return nil
}

// Binds every function variable of the table, binding none when the library lacks one
func (l *Library) BindAll(table map[string]any) error {
	addrs := map[string]uintptr{}
	for name := range table {
		addr, err := l.Symbol(name)
		if err != nil {
			return err
		}
		addrs[name] = addr
	}
	for name, fptr := range table {
		RegisterFunc(fptr, addrs[name])
	}
	return nil
}

func text(err error, fallback string) string {
	if err == nil || err.Error() == "" {
		return fallback
	}
	return err.Error()
}

func subject(message, name string) string {
	if name == "" || strings.Contains(message, name) {
		return message
	}
	return name + ": " + message
}

// NUL terminated copy of s for a char pointer argument, kept referenced as long as C reads it
func CString(s string) []byte {
	out := make([]byte, len(s)+1)
	copy(out, s)
	return out
}

// Pointer to the first byte of a NUL terminated copy, nil for the empty string
func Chars(held *[][]byte, s string) *byte {
	if s == "" {
		return nil
	}
	buf := CString(s)
	*held = append(*held, buf)
	return &buf[0]
}

// Copy of the NUL terminated C string at an address, empty for a null pointer
func GoString(p uintptr) string {
	if p == 0 {
		return ""
	}
	base := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	n := 0
	for *(*byte)(unsafe.Add(base, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(base), n))
}

// Copy of a C string a field points at, empty for nil
func Text(p *byte) string { return GoString(uintptr(unsafe.Pointer(p))) }

// Pointer sized value at index i of a C array of pointers
func PointerAt(array uintptr, i int) uintptr {
	base := *(*unsafe.Pointer)(unsafe.Pointer(&array))
	return *(*uintptr)(unsafe.Add(base, uintptr(i)*unsafe.Sizeof(uintptr(0))))
}

// Typed pointer of a C address
func At[T any](addr uintptr) *T { return (*T)(*(*unsafe.Pointer)(unsafe.Pointer(&addr))) }

// Copy of n float samples of library memory
func Floats(addr uintptr, n int32) []float32 {
	if addr == 0 || n <= 0 {
		return nil
	}
	return append([]float32(nil), unsafe.Slice(At[float32](addr), int(n))...)
}

// Soname or path the platform loader resolves for a short library name such as portaudio, empty when none is installed
func Find(name string) string {
	if name == "" {
		return ""
	}
	return find(name)
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Entries of a directory list an environment variable holds
func envDirs(key string) []string {
	var dirs []string
	for _, dir := range filepath.SplitList(os.Getenv(key)) {
		if dir != "" {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
