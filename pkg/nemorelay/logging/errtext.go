package logging

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// Operating system error rendered the way the platform message and os error code read
func osErrorText(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	text := errno.Error()
	if runtime.GOOS != "windows" {
		if r, size := utf8.DecodeRuneInString(text); r != utf8.RuneError {
			text = string(unicode.ToUpper(r)) + text[size:]
		}
	}
	return fmt.Sprintf("%s (os error %d)", strings.TrimRight(text, "\r\n"), uintptr(errno))
}

// Short reason naming why a file could not open
func fileOpenFailureReason(err error) string {
	var errno syscall.Errno
	switch {
	case errors.Is(err, os.ErrPermission):
		return "Logging sink could not open: permission_denied"
	case errors.Is(err, os.ErrNotExist):
		return "Logging sink could not open: not_found"
	case errors.As(err, &errno):
		if reason := errnoReason(errno); reason != "" {
			return "Logging sink could not open: " + reason
		}
	}
	return "Logging sink could not open: io_error"
}
