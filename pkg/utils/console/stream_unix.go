//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || illumos || aix

package consoleutil

import (
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Standard descriptors a live region points at pipes while it runs: output and error
var standardFds = []int{1, 2}

// Points descriptor fd at a new pipe, keeping a close on exec duplicate of what it pointed at
func captureStream(fd int) (*capturedStream, error) {
	syscall.ForkLock.RLock()
	saved, err := syscall.Dup(fd)
	if err == nil {
		syscall.CloseOnExec(saved)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, err
	}
	name := "stdout"
	if fd == 2 {
		name = "stderr"
	}
	term := os.NewFile(uintptr(saved), name)
	r, w, err := os.Pipe()
	if err != nil {
		term.Close()
		return nil, err
	}
	if err := dup2(int(w.Fd()), fd); err != nil {
		r.Close()
		w.Close()
		term.Close()
		return nil, err
	}
	w.Close()
	return &capturedStream{fd: fd, term: term, termFd: uintptr(saved), read: r}, nil
}

// Points the descriptor back at the terminal it pointed at before the capture
func (s *capturedStream) release() error { return dup2(int(s.termFd), s.fd) }

// Points descriptor to at what descriptor from points at, retrying when a signal interrupts the call
func dup2(from, to int) error {
	for {
		if err := unix.Dup2(from, to); err != unix.EINTR {
			return err
		}
	}
}

// Whether two descriptors are the same terminal device
func sameTerminal(a, b uintptr) bool {
	var sa, sb unix.Stat_t
	if unix.Fstat(int(a), &sa) != nil || unix.Fstat(int(b), &sb) != nil {
		return false
	}
	return sa.Mode&unix.S_IFMT == unix.S_IFCHR && sb.Mode&unix.S_IFMT == unix.S_IFCHR && sa.Rdev == sb.Rdev
}

// Whether escape sequences written to w take effect, always on Unix terminals
func escapesWork(io.Writer) bool { return true }
