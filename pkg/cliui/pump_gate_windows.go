package cliui

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// Readable wait on a console input handle that its wake event ends
type handleGate struct {
	input, wake windows.Handle
}

// Gate of a console backed stream, nil for any other reader or when no wake event can be made
func newReadGate(r io.Reader) readGate {
	f, isFile := r.(*os.File)
	if !isFile {
		return nil
	}
	input := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(input, &mode) != nil {
		return nil
	}
	wake, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil
	}
	return &handleGate{input: input, wake: wake}
}

func (g *handleGate) wait() bool {
	fired, err := windows.WaitForMultipleObjects([]windows.Handle{g.wake, g.input}, false, windows.INFINITE)
	return err != nil || fired != windows.WAIT_OBJECT_0
}

func (g *handleGate) release() {
	_ = windows.SetEvent(g.wake)
}
