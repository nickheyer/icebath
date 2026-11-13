//go:build !((darwin || freebsd || linux || netbsd || windows) && !386 && !arm)

package dl

import (
	"fmt"
	"runtime"
)

// Whether this build can open shared libraries
const Supported = false

// Refuses every library because this build has no dynamic loader binding
func Open(path string, _ Scope) (*Library, error) {
	return nil, fmt.Errorf("open %s on %s/%s: %w", path, runtime.GOOS, runtime.GOARCH, ErrUnsupported)
}

// Refuses the search because no library is loadable without a dynamic loader binding
func Find(name string) (string, error) {
	return "", fmt.Errorf("find %s on %s/%s: %w", name, runtime.GOOS, runtime.GOARCH, ErrUnsupported)
}

// Refuses because nothing on this build could ever call a C function pointer
func Callback(fn any) (uintptr, error) {
	checkCallback(fn)
	return 0, fmt.Errorf("callback on %s/%s: %w", runtime.GOOS, runtime.GOARCH, ErrUnsupported)
}

// Unreachable because every symbol lookup on this build fails, so no function address exists to pass here
func Call(uintptr, ...uintptr) (r1, errno uintptr) { panic(ErrUnsupported) }

// Unreachable because every symbol lookup on this build fails, so no function address exists to pass here
func CallVariadic(uintptr, int, ...uintptr) (r1, errno uintptr) { panic(ErrUnsupported) }

func lookup(uintptr, string) (uintptr, error) { return 0, ErrUnsupported }

// Unreachable because every symbol lookup on this build fails before a binding is made
func register(any, uintptr) { panic(ErrUnsupported) }
