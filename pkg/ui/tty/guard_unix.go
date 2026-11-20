//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || illumos || aix

package tty

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// Second descriptor for the stderr the program started with
func dupStderr() (*os.File, error) {
	fd, err := unix.Dup(2)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "stderr"), nil
}

// Points descriptor 2 at the capture pipe and closes the pipe end, the runtime's own crash reports landing there too
func redirectStderr(w *os.File) error {
	return errors.Join(unix.Dup2(int(w.Fd()), 2), w.Close())
}

// Points descriptor 2 back at the stderr the program started with and closes the second descriptor
func restoreStderr(real *os.File) error {
	return errors.Join(unix.Dup2(int(real.Fd()), 2), real.Close())
}

// Starts the guard with the files at descriptors 3 onward, in the program's process group so job control stops both
func spawnGuard(cmd *exec.Cmd, files []*os.File) error {
	cmd.ExtraFiles = files
	return cmd.Start()
}

// Descriptors the guard was started with
func guardFiles() (ctrl, ack, capture, real, in, out *os.File, err error) {
	f := func(fd uintptr, name string) *os.File { return os.NewFile(fd, name) }
	return f(3, "ctrl"), f(4, "ack"), f(5, "capture"), f(6, "stderr"), f(7, "in"), f(8, "out"), nil
}

// Keeps the guard alive through the signals meant for the program
func ignoreSignals() {
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU, syscall.SIGWINCH, syscall.SIGPIPE)
}

// Termios of the input end as the program found it
func (t *ttyState) saveOrig() error {
	tio, err := unix.IoctlGetTermios(int(t.in.Fd()), ioctlGetTermios)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := binary.Write(&b, binary.NativeEndian, tio); err != nil {
		return err
	}
	t.orig = b.Bytes()
	return nil
}

// Termios bytes the guard puts back
func saveModes(st *ttyState) []byte { return st.orig }

// Puts the saved termios back on the input end
func loadModes(in, _ *os.File, modes []byte) error {
	var tio unix.Termios
	if err := binary.Read(bytes.NewReader(modes), binary.NativeEndian, &tio); err != nil {
		return errGuardModes
	}
	return unix.IoctlSetTermios(int(in.Fd()), ioctlSetTermios, &tio)
}
