package pypath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Python str of the OSError a Go error stands for, errno tag, strerror and quoted filenames
func OSErrorText(err error) string {
	var ee *exec.Error
	if errors.As(err, &ee) && errors.Is(ee.Err, exec.ErrNotFound) {
		if windows {
			return "[WinError 2] The system cannot find the file specified"
		}
		return "[Errno 2] No such file or directory: " + pyrepr.StrRepr(ee.Name)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		if text, ok := ErrnoText(pe.Err); ok {
			if windows && pe.Op == "fork/exec" {
				return text
			}
			return text + ": " + pyrepr.StrRepr(pe.Path)
		}
		return err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		if text, ok := ErrnoText(le.Err); ok {
			return text + ": " + pyrepr.StrRepr(le.Old) + " -> " + pyrepr.StrRepr(le.New)
		}
		return err.Error()
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		var errno syscall.Errno
		if errors.As(se.Err, &errno) {
			return fmt.Sprintf("[Errno %d] (originated from %s)", uint(errno), se.Syscall)
		}
		return err.Error()
	}
	if text, ok := ErrnoText(err); ok {
		return text
	}
	return err.Error()
}

// Bracketed errno and C strerror text of a syscall error, WinError and its trimmed message on Windows
func ErrnoText(err error) (string, bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "", false
	}
	msg := errno.Error()
	if windows {
		msg = strings.TrimRightFunc(msg, func(r rune) bool { return r <= ' ' || r == '.' })
		return fmt.Sprintf("[WinError %d] %s", uint(errno), msg), true
	}
	return fmt.Sprintf("[Errno %d] %s", uint(errno), Strerror(errno)), true
}

// C strerror text of an errno, the Go message with its first letter raised
func Strerror(errno syscall.Errno) string {
	msg := []rune(errno.Error())
	if len(msg) > 0 {
		msg[0] = unicode.ToUpper(msg[0])
	}
	return string(msg)
}

// PEP 3151 OSError subclass name for the errno a Go error carries
func OSErrorClass(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, exec.ErrNotFound):
			return "FileNotFoundError"
		case errors.Is(err, fs.ErrExist):
			return "FileExistsError"
		case errors.Is(err, fs.ErrPermission):
			return "PermissionError"
		}
		return "OSError"
	}
	if name, ok := errnoClasses[errno]; ok {
		return name
	}
	return "OSError"
}

// Python errno.errorcode name of the errno the OSError a Go error stands for would carry
func ErrnoName(err error) (string, bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "", false
	}
	name := errnoName(errno)
	return name, name != ""
}
