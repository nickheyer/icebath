//go:build windows

package tools

import (
	"os"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// Reads one line from the console with echo disabled, empty on control C or any failure
func readHiddenLine() string {
	name, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return ""
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(handle)
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return ""
	}
	hidden := (mode | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT) &^ windows.ENABLE_ECHO_INPUT
	if err := windows.SetConsoleMode(handle, hidden); err != nil {
		return ""
	}
	defer windows.SetConsoleMode(handle, mode)
	var chars []uint16
	buf := make([]uint16, 1)
	for {
		var read uint32
		if err := windows.ReadConsole(handle, &buf[0], 1, &read, nil); err != nil || read == 0 {
			break
		}
		c := buf[0]
		if c == '\r' || c == '\n' {
			break
		}
		if c == 0x03 {
			return ""
		}
		chars = append(chars, c)
	}
	return string(utf16.Decode(chars))
}

// Flushes stdout the way the hermes prompt does after every status line
func flushStdout() {
	os.Stdout.Sync()
}
