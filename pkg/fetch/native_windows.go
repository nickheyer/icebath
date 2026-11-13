//go:build windows && !386 && !arm

package fetch

import (
	"fmt"
	"path/filepath"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/windows"
)

// Whether this build can load native libraries
const NativeSupported = true

// Loads a DLL searching its own directory first for the libraries it depends on
func OpenLibrary(path string) (*Library, error) {
	full, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.LoadLibraryEx(full, 0, windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS|windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR)
	if err != nil {
		return nil, fmt.Errorf("loading %s failed: %w", full, err)
	}
	return &Library{Path: full, handle: uintptr(handle)}, nil
}

func symbol(handle uintptr, name string) (uintptr, error) {
	return windows.GetProcAddress(windows.Handle(handle), name)
}

func registerFunc(fn any, addr uintptr) { purego.RegisterFunc(fn, addr) }
