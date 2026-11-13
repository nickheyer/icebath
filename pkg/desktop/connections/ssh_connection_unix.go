//go:build !windows

package connections

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Exit code and signal name of a finished child, the code nil for a signal death
func processExit(state *os.ProcessState) (*int, string) {
	if state == nil {
		return nil, ""
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return nil, unix.SignalName(ws.Signal())
	}
	code := state.ExitCode()
	return &code, ""
}

// SIGTERM the way Node child.kill sends it
func terminateChild(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// Node style errno code of a syscall error
func errnoCode(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if name := unix.ErrnoName(errno); name != "" {
			return name
		}
	}
	return "undefined"
}

// The resolved parent of the short control dir is root or user owned and sticky when writable
func checkShortControlParent() error {
	parent := filepath.Dir(shortControlDir())
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	info, err := os.Stat(real)
	if err != nil {
		return err
	}
	st, _ := info.Sys().(*syscall.Stat_t)
	uid := -1
	if st != nil {
		uid = int(st.Uid)
	}
	mode := uint32(info.Mode().Perm())
	if info.Mode()&os.ModeSticky != 0 {
		mode |= 0o1000
	}
	if !info.IsDir() || (uid != 0 && uid != os.Getuid()) || (mode&0o022 != 0 && mode&0o1000 == 0) {
		return fmt.Errorf("Unsafe SSH control parent: %s must be owned by root or this user and sticky if writable.", parent)
	}
	return nil
}

// The control dir is a real directory owned by this user with mode 0700
func secureControlDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, _ := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || st == nil || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("Unsafe SSH control dir: %s is not a directory owned by this user (no symlinks).", dir)
	}
	if info.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}
