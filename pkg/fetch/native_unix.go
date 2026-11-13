//go:build (darwin || freebsd || linux || netbsd) && !386 && !arm && !faketime

package fetch

import (
	"fmt"

	"github.com/ebitengine/purego"
)

// Whether this build can load native libraries
const NativeSupported = true

// Loads a native library with immediate binding, its symbols visible to libraries loaded after it
func OpenLibrary(path string) (*Library, error) {
	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("loading %s failed: %w", path, err)
	}
	return &Library{Path: path, handle: handle}, nil
}

func symbol(handle uintptr, name string) (uintptr, error) { return purego.Dlsym(handle, name) }

func registerFunc(fn any, addr uintptr) { purego.RegisterFunc(fn, addr) }
