//go:build windows

package pm

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// Seam pointing stdout at stderr while a side environment builds, swapped by tests
var redirectStdout = redirectStdoutToStderr

// Reports a handle the C runtime isatty answers true for, any character device such as a console or NUL
func fileIsTTY(f *os.File) bool {
	kind, err := windows.GetFileType(windows.Handle(f.Fd()))
	return err == nil && kind == windows.FILE_TYPE_CHAR
}

// Opts a console handle in to VT processing, false for a handle that is no console
func enableVTProcessing(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || f == nil {
		return false
	}
	handle := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(handle, &mode) != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}

// Points the standard output handle at stderr so children print there too, returning the restore
func redirectStdoutToStderr() (func(), error) {
	saved, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return nil, err
	}
	if err := windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(os.Stderr.Fd())); err != nil {
		return nil, err
	}
	stdout := os.Stdout
	os.Stdout = os.Stderr
	return func() {
		_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, saved)
		os.Stdout = stdout
	}, nil
}
