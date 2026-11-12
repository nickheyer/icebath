//go:build windows

package relayhost

import (
	"os"

	"golang.org/x/sys/windows"
)

// Keeps stdin and stdout for the channel and points the standard handles at NUL and stderr first
func takeProtocolStreams() (*os.File, *os.File, error) {
	in, out := os.Stdin, os.Stdout
	null, err := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := windows.SetStdHandle(windows.STD_INPUT_HANDLE, null); err != nil {
		return nil, nil, err
	}
	if err := windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(os.Stderr.Fd())); err != nil {
		return nil, nil, err
	}
	os.Stdin = os.NewFile(uintptr(null), "NUL")
	os.Stdout = os.Stderr
	return in, out, nil
}
