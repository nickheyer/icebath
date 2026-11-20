//go:build windows && !386 && !arm

package pyctypes

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Whether this build can load shared libraries
const Supported = true

// Loads a DLL the way ctypes CDLL does, the DLL's own directory searched when the name carries a path
func LoadLibrary(path string, global bool) (*CDLL, error) {
	flags := uintptr(windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS)
	name := path
	if strings.ContainsAny(name, `/\`) {
		if full, err := filepath.Abs(name); err == nil {
			name = full
		}
		flags |= windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR
	}
	handle, err := windows.LoadLibraryEx(name, 0, flags)
	if err != nil {
		return nil, &OSError{Msg: fmt.Sprintf("Could not find module '%s' (or one of its dependencies). Try using the full path with constructor syntax.", name)}
	}
	return &CDLL{Name: name, handle: uintptr(handle)}, nil
}

// Address of an exported symbol, an AttributeError naming the function when absent
func (l *CDLL) Symbol(name string) (uintptr, error) {
	addr, err := windows.GetProcAddress(windows.Handle(l.handle), name)
	if err != nil || addr == 0 {
		return 0, &AttributeError{Msg: fmt.Sprintf("function '%s' not found", name)}
	}
	return addr, nil
}

// Binds a Go function variable to a C function address
func RegisterFunc(fptr any, addr uintptr) { purego.RegisterFunc(fptr, addr) }

// Adds a directory to the DLL search path until the returned close runs, an OSError naming the path on failure
func AddDLLDirectory(path string) (func(), error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &OSError{Msg: "embedded null character in path"}
	}
	cookie, err := windows.AddDllDirectory(p)
	if err != nil {
		return nil, &OSError{Msg: pypath.OSErrorText(err) + ": " + pyrepr.StrRepr(path)}
	}
	return func() { _ = windows.RemoveDllDirectory(cookie) }, nil
}
