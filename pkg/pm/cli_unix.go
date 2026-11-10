//go:build !windows

package pm

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/unix"
)

// Seam pointing stdout at stderr while a side environment builds, swapped by tests
var redirectStdout = redirectStdoutToStderr

// Reports a file the C library isatty answers true for, a terminal device
func fileIsTTY(f *os.File) bool { return isatty.IsTerminal(f.Fd()) }

// Terminals outside Windows render escape sequences without opting in
func enableVTProcessing(w io.Writer) bool { return true }

// Points descriptor 1 at stderr so children print there too, returning the restore
func redirectStdoutToStderr() (func(), error) {
	saved, err := unix.Dup(1)
	if err != nil {
		return nil, err
	}
	if err := unix.Dup2(2, 1); err != nil {
		_ = unix.Close(saved)
		return nil, err
	}
	return func() {
		_ = unix.Dup2(saved, 1)
		_ = unix.Close(saved)
	}, nil
}
