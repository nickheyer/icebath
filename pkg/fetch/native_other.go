//go:build !((darwin || freebsd || linux || netbsd || windows) && !386 && !arm && !faketime)

package fetch

import (
	"fmt"
	"runtime"
)

// Whether this build can load native libraries
const NativeSupported = false

// Refuses to load, this build has no dynamic loader binding
func OpenLibrary(path string) (*Library, error) {
	return nil, fmt.Errorf("%s cannot be loaded, this build has no dynamic loader for %s/%s", path, runtime.GOOS, runtime.GOARCH)
}

func symbol(uintptr, string) (uintptr, error) {
	return 0, fmt.Errorf("no dynamic loader for %s/%s", runtime.GOOS, runtime.GOARCH)
}

func registerFunc(any, uintptr) {
	panic("no dynamic loader for " + runtime.GOOS + "/" + runtime.GOARCH)
}
