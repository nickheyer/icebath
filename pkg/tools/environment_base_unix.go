//go:build !windows

package tools

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Takes an flock on the lock file within the timeout and returns the release function
func lockStoreFile(lockPath string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, errors.New("Timed out waiting for the snapshot store lock (" + lockPath + ")")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Inode and ctime of a stat result
func statIdentity(st os.FileInfo) (uint64, int64) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Ino), statCtimeNanos(sys)
	}
	return 0, 0
}

// Console hiding is a Windows concern only
func hideConsoleWindow(cmd *exec.Cmd) {}

// Signal number that terminated the process, zero when it exited normally
func signalOf(state *os.ProcessState) int {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return int(ws.Signal())
	}
	return 0
}

// Reports a write to a pipe whose reader is gone
func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE)
}

// Reads one chunk within the timeout, ready false on an idle pipe, error on EOF or a closed file
func readChunkTimeout(f *os.File, buf []byte, timeout time.Duration) (int, bool, error) {
	if err := f.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		n, rerr := f.Read(buf)
		if n > 0 {
			return n, true, nil
		}
		if rerr == nil {
			rerr = io.EOF
		}
		return 0, false, rerr
	}
	n, err := f.Read(buf)
	if n > 0 {
		return n, true, nil
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return 0, false, nil
	}
	if err == nil {
		err = io.EOF
	}
	return 0, false, err
}
