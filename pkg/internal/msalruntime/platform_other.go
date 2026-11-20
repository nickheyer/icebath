//go:build !(((darwin || linux) && (amd64 || arm64) && !faketime) || (windows && (amd64 || arm64 || 386)))

package msalruntime

import (
	"errors"
	"runtime"
)

// Whether this build can load the native library
const loadSupported = false

var errNoLoader = errors.New("no msalruntime loader exists for " + runtime.GOOS + "/" + runtime.GOARCH)

func openLibrary(string) (uintptr, error) { return 0, errNoLoader }

func lookupSymbol(uintptr, string) (uintptr, error) { return 0, errNoLoader }

// Load refuses this platform before any call is made, so no C function is ever reached here
func call(uintptr, ...uintptr) uintptr { return 0 }

func newCompletionCallback(func(result, data uintptr)) uintptr { return 0 }

func newLogCallback(func(message uintptr, level int32, data uintptr)) uintptr { return 0 }
