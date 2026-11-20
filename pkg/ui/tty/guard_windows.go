//go:build windows

package tty

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Environment variable naming the handles the guard inherits
const guardHandlesEnv = "ICEBATH_TTY_GUARD_HANDLES"

// Second handle for the stderr the program started with
func dupStderr() (*os.File, error) {
	h, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil {
		return nil, err
	}
	var dup windows.Handle
	self := windows.CurrentProcess()
	if err := windows.DuplicateHandle(self, h, self, &dup, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(dup), "stderr"), nil
}

// Points the stderr handle and os.Stderr at the capture pipe, taking the pipe over, the runtime's crash reports landing there too
func redirectStderr(w *os.File) error {
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(w.Fd())); err != nil {
		return errors.Join(err, w.Close())
	}
	os.Stderr = w
	return nil
}

// Points stderr back at the handle the program started with, closing the capture pipe end it replaces
func restoreStderr(real *os.File) error {
	err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(real.Fd()))
	if old := os.Stderr; old != real {
		os.Stderr = real
		err = errors.Join(err, old.Close())
	}
	return err
}

// Starts the guard inheriting the handles, their values passed in the environment
func spawnGuard(cmd *exec.Cmd, files []*os.File) error {
	var hs []string
	var inherit []syscall.Handle
	for _, f := range files {
		h := windows.Handle(f.Fd())
		if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return err
		}
		inherit = append(inherit, syscall.Handle(h))
		hs = append(hs, strconv.FormatUint(uint64(h), 10))
	}
	cmd.Env = append(cmd.Env, guardHandlesEnv+"="+strings.Join(hs, ","))
	cmd.SysProcAttr = &syscall.SysProcAttr{AdditionalInheritedHandles: inherit}
	return cmd.Start()
}

// Handles the guard was started with, an error when the environment does not name all six
func guardFiles() (ctrl, ack, capture, real, in, out *os.File, err error) {
	parts := strings.Split(os.Getenv(guardHandlesEnv), ",")
	if len(parts) != 6 {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("tty: guard handles %q", os.Getenv(guardHandlesEnv))
	}
	files := make([]*os.File, 6)
	for i, name := range []string{"ctrl", "ack", "capture", "stderr", "in", "out"} {
		h, err := strconv.ParseUint(parts[i], 10, 64)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, fmt.Errorf("tty: guard handle %s: %w", name, err)
		}
		files[i] = os.NewFile(uintptr(h), name)
	}
	return files[0], files[1], files[2], files[3], files[4], files[5], nil
}

// Keeps the guard alive through the console events meant for the program
func ignoreSignals() { signal.Ignore(os.Interrupt, syscall.SIGTERM) }

// Console modes of both ends as the program found them
func (t *ttyState) saveOrig() error {
	var in, out uint32
	if err := windows.GetConsoleMode(windows.Handle(t.in.Fd()), &in); err != nil {
		return err
	}
	if err := windows.GetConsoleMode(windows.Handle(t.out.Fd()), &out); err != nil {
		return err
	}
	t.orig = binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, in), out)
	return nil
}

// Console mode bytes the guard puts back
func saveModes(st *ttyState) []byte { return st.orig }

// Puts the saved console modes back on both ends
func loadModes(in, out *os.File, modes []byte) error {
	if len(modes) != 8 {
		return fmt.Errorf("%w: %d bytes", errGuardModes, len(modes))
	}
	if err := windows.SetConsoleMode(windows.Handle(in.Fd()), binary.BigEndian.Uint32(modes)); err != nil {
		return err
	}
	return windows.SetConsoleMode(windows.Handle(out.Fd()), binary.BigEndian.Uint32(modes[4:]))
}
