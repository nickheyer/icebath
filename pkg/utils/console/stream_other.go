//go:build !windows && !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || illumos || aix)

package consoleutil

import (
	"errors"
	"io"
)

// Standard descriptors a live region points at pipes, none where the platform cannot point a descriptor elsewhere
var standardFds []int

func captureStream(int) (*capturedStream, error) { return nil, errors.ErrUnsupported }

func (s *capturedStream) release() error { return nil }

// Whether two descriptors are the same terminal, known here only when they are the same descriptor
func sameTerminal(a, b uintptr) bool { return a == b }

// Whether escape sequences written to w take effect
func escapesWork(io.Writer) bool { return true }
