package pysys

import (
	"io"
	"os"
)

// Writer resolving the process stdout at each write, the sys.stdout a redirect swaps
var Stdout io.Writer = stdout{}

// Writer resolving the process stderr at each write, the sys.stderr a redirect swaps
var Stderr io.Writer = stderr{}

type stdout struct{}

// Writes p to the current process stdout
func (stdout) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

// Descriptor of the current process stdout for terminal probes
func (stdout) Fd() uintptr { return os.Stdout.Fd() }

type stderr struct{}

// Writes p to the current process stderr
func (stderr) Write(p []byte) (int, error) { return os.Stderr.Write(p) }

// Descriptor of the current process stderr for terminal probes
func (stderr) Fd() uintptr { return os.Stderr.Fd() }
