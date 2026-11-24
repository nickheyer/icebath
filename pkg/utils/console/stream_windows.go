package consoleutil

import (
	"errors"
	"io"

	termutil "github.com/nickheyer/icebath/pkg/utils/term"
	"golang.org/x/sys/windows"
)

// Standard descriptors a live region points at pipes, none on Windows: os.Stdout and os.Stderr hold their console handles, so only changing those variables could reroute them
var standardFds []int

func captureStream(int) (*capturedStream, error) { return nil, errors.ErrUnsupported }

func (s *capturedStream) release() error { return nil }

// Whether two handles are consoles, which in one process are the same console
func sameTerminal(a, b uintptr) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(a), &mode) == nil && windows.GetConsoleMode(windows.Handle(b), &mode) == nil
}

// Whether escape sequences written to w take effect: anything but a console takes them as written, a console once virtual terminal processing is on, which this switches on when it is off
func escapesWork(w io.Writer) bool {
	fd, ok := termutil.Fd(w)
	if !ok {
		return true
	}
	h := windows.Handle(fd)
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil || mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
