package pysys

import (
	"io"
	"os"
	"sync/atomic"
)

// Writer resolving the process stdout at each write, the sys.stdout a redirect swaps
var Stdout io.Writer = stdout{}

// Writer resolving the process stderr at each write, the sys.stderr a redirect swaps
var Stderr io.Writer = stderr{}

// Writers a running prompt put in place of the process stdout and stderr, nil while none is
var patchedOut, patchedErr atomic.Pointer[io.Writer]

// Routes Stdout and Stderr through out and err until the returned restore runs
func Patch(out, err io.Writer) (restore func()) {
	prevOut, prevErr := patchedOut.Swap(&out), patchedErr.Swap(&err)
	return func() {
		patchedOut.Store(prevOut)
		patchedErr.Store(prevErr)
	}
}

type stdout struct{}

// Writes p to the patched stdout, else the current process stdout
func (stdout) Write(p []byte) (int, error) {
	if w := patchedOut.Load(); w != nil {
		return (*w).Write(p)
	}
	return os.Stdout.Write(p)
}

// Descriptor of the current process stdout for terminal probes
func (stdout) Fd() uintptr { return os.Stdout.Fd() }

type stderr struct{}

// Writes p to the patched stderr, else the current process stderr
func (stderr) Write(p []byte) (int, error) {
	if w := patchedErr.Load(); w != nil {
		return (*w).Write(p)
	}
	return os.Stderr.Write(p)
}

// Descriptor of the current process stderr for terminal probes
func (stderr) Fd() uintptr { return os.Stderr.Fd() }
