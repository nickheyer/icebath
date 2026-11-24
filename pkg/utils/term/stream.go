package termutil

import (
	"io"
	"os"
	"runtime"
	"syscall"

	"github.com/mattn/go-isatty"
)

// Longest Unwrap chain followed, a guard against a wrapper that unwraps to itself
const maxUnwrap = 64

// Next stream down an Unwrap() io.Writer or Unwrap() io.Reader chain, nil at its end
func unwrap(stream any) any {
	switch s := stream.(type) {
	case interface{ Unwrap() io.Writer }:
		return s.Unwrap()
	case interface{ Unwrap() io.Reader }:
		return s.Unwrap()
	}
	return nil
}

// Capability T of a stream: the stream itself when it has it, else the first stream down its Unwrap() io.Writer or Unwrap() io.Reader chain that does
func As[T any](stream any) (T, bool) {
	for range maxUnwrap {
		if stream == nil {
			break
		}
		if v, ok := stream.(T); ok {
			return v, true
		}
		stream = unwrap(stream)
	}
	var zero T
	return zero, false
}

// Descriptor of one stream, not looking down its chain: read through SyscallConn, which leaves a non blocking file non blocking and a Windows handle on the runtime's completion port, else its Fd method; an *os.File that is nil or closed has none
func ownFd(stream any) (uintptr, bool) {
	if c, ok := stream.(syscall.Conn); ok {
		if rc, err := c.SyscallConn(); err == nil {
			var fd uintptr
			found := false
			if rc.Control(func(d uintptr) { fd, found = d, true }) == nil && found {
				return fd, true
			}
		}
		if _, file := stream.(*os.File); file && runtime.GOOS != "plan9" {
			return 0, false
		}
	}
	if f, ok := stream.(interface{ Fd() uintptr }); ok {
		return f.Fd(), true
	}
	return 0, false
}

// Descriptor of a stream or of the first stream it wraps that has one, false when none has one; an *os.File is asked through SyscallConn, so its blocking mode and deadlines stay as they are
func Fd(stream any) (uintptr, bool) {
	for range maxUnwrap {
		if stream == nil {
			break
		}
		if fd, ok := ownFd(stream); ok {
			return fd, true
		}
		stream = unwrap(stream)
	}
	return 0, false
}

// Descriptor of a file for its Fd method, the invalid descriptor when the file is nil or closed
func fileFd(f *os.File) uintptr {
	if fd, ok := ownFd(f); ok {
		return fd
	}
	return ^uintptr(0)
}

// Whether a stream is a terminal: the IsTerminal() bool answer of the stream or of what it wraps, else whether its descriptor is a terminal or a Cygwin or MSYS terminal pipe
func IsTerminal(stream any) bool {
	if t, ok := As[interface{ IsTerminal() bool }](stream); ok {
		return t.IsTerminal()
	}
	fd, ok := Fd(stream)
	return ok && (isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd))
}
