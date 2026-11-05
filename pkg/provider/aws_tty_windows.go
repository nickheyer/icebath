//go:build windows

package provider

import (
	"errors"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// Writes the prompt to the console and reads one line with echo off
func awsReadHiddenLine(prompt string) (string, error) {
	in, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(in, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return "", errors.New("provider: no console to read the MFA code from")
	}
	defer windows.CloseHandle(handle)
	if out, err := windows.UTF16PtrFromString("CONOUT$"); err == nil {
		if oh, err := windows.CreateFile(out, windows.GENERIC_WRITE, windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0); err == nil {
			p := utf16.Encode([]rune(prompt))
			var written uint32
			_ = windows.WriteConsole(oh, &p[0], uint32(len(p)), &written, nil)
			defer windows.CloseHandle(oh)
		}
	}
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return "", err
	}
	hidden := (mode | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT) &^ windows.ENABLE_ECHO_INPUT
	if err := windows.SetConsoleMode(handle, hidden); err != nil {
		return "", err
	}
	defer windows.SetConsoleMode(handle, mode)
	var chars []uint16
	buf := make([]uint16, 1)
	for {
		var read uint32
		if err := windows.ReadConsole(handle, &buf[0], 1, &read, nil); err != nil || read == 0 {
			break
		}
		if buf[0] == '\r' || buf[0] == '\n' {
			break
		}
		if buf[0] == 0x03 {
			return "", errors.New("provider: MFA prompt interrupted")
		}
		chars = append(chars, buf[0])
	}
	return string(utf16.Decode(chars)), nil
}
