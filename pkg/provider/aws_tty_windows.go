//go:build windows

package provider

import (
	"context"
	"errors"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Console input record as ReadConsoleInputW fills it, the union read as a key event
type awsConsoleKeyRecord struct {
	eventType   uint16
	_           uint16
	keyDown     int32
	repeatCount uint16
	_           [2]uint16
	unicodeChar uint16
	_           uint32
}

// Console input reader x/sys/windows leaves unwrapped
var awsReadConsoleInput = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleInputW")

// Reads one hidden line key by key like win_getpass, checking ctx every tenth of a second
func awsReadHiddenLine(ctx context.Context, prompt string) (string, error) {
	in, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(in, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return "", errors.New("provider: no console to read the MFA code from")
	}
	defer windows.CloseHandle(handle)
	write := func(string) {}
	if out, err := windows.UTF16PtrFromString("CONOUT$"); err == nil {
		if oh, err := windows.CreateFile(out, windows.GENERIC_WRITE, windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0); err == nil {
			defer windows.CloseHandle(oh)
			write = func(text string) {
				p := utf16.Encode([]rune(text))
				var written uint32
				_ = windows.WriteConsole(oh, &p[0], uint32(len(p)), &written, nil)
			}
		}
	}
	write(prompt)
	defer write("\r\n")
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return "", err
	}
	if err := windows.SetConsoleMode(handle, mode&^(windows.ENABLE_LINE_INPUT|windows.ENABLE_ECHO_INPUT|windows.ENABLE_PROCESSED_INPUT)); err != nil {
		return "", err
	}
	defer windows.SetConsoleMode(handle, mode)
	var chars []uint16
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		event, err := windows.WaitForSingleObject(handle, 100)
		if err != nil {
			return "", err
		}
		if event == uint32(windows.WAIT_TIMEOUT) {
			continue
		}
		var rec awsConsoleKeyRecord
		var read uint32
		if ok, _, err := awsReadConsoleInput.Call(uintptr(handle), uintptr(unsafe.Pointer(&rec)), 1, uintptr(unsafe.Pointer(&read))); ok == 0 {
			return "", err
		}
		if read == 0 || rec.eventType != windows.KEY_EVENT || rec.keyDown == 0 || rec.unicodeChar == 0 {
			continue
		}
		for range max(rec.repeatCount, 1) {
			switch rec.unicodeChar {
			case '\r', '\n':
				return string(utf16.Decode(chars)), nil
			case 0x03:
				return "", errors.New("provider: MFA prompt interrupted")
			case '\b':
				if len(chars) > 0 {
					chars = chars[:len(chars)-1]
				}
			default:
				chars = append(chars, rec.unicodeChar)
			}
		}
	}
}
