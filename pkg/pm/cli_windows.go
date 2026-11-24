//go:build windows && !icebath_core && !icebath_noextras

package pm

import (
	"io"
	"os"

	termutil "github.com/nickheyer/icebath/pkg/utils/term"
	"golang.org/x/sys/windows"
)

// Seam pointing stdout at stderr while a side environment builds, swapped by tests
var redirectStdout = redirectStdoutToStderr

// Opts a console handle in to VT processing, a writer without a handle rendering escapes when it reports a terminal
func enableVTProcessing(w io.Writer) bool {
	fd, ok := termutil.Fd(w)
	if !ok {
		return termutil.IsTerminal(w)
	}
	handle := windows.Handle(fd)
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
