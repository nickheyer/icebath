package cliui

import (
	"os"

	"golang.org/x/sys/windows"
)

// Windows delivers no terminating signal to a console program, so a hold has nothing to watch
func watchTermination(*termGuard) func() { return func() {} }

// Windows consoles have no job control stop, so a stop request leaves the prompt running
func (c *Console) suspend() bool { return false }

// Stops the console echoing typed keys, leaving every other mode as it is
func muteEcho(f *os.File) {
	handle := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(handle, &mode) == nil {
		windows.SetConsoleMode(handle, mode&^windows.ENABLE_ECHO_INPUT)
	}
}
