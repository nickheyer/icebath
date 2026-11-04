//go:build windows

package tools

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Takes a LockFileEx lock on the lock file within the timeout and returns the release function
func lockStoreFile(lockPath string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	h := windows.Handle(f.Fd())
	for {
		ol := new(windows.Overlapped)
		err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			return func() {
				windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
				f.Close()
			}, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
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

// Inode and creation time of a stat result, Windows reports the creation time as ctime
func statIdentity(st os.FileInfo) (uint64, int64) {
	if sys, ok := st.Sys().(*syscall.Win32FileAttributeData); ok {
		return 0, sys.CreationTime.Nanoseconds()
	}
	return 0, 0
}

// Starts the child without a console window
func hideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}

// Windows processes never report a terminating signal
func signalOf(state *os.ProcessState) int { return 0 }

// Reports a write to a pipe whose reader is gone
func isBrokenPipe(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA)
}

// PeekNamedPipe from kernel32 for pipe readiness polling
var procPeekNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

// Bytes available on the pipe without consuming them
func peekNamedPipe(h windows.Handle) (uint32, error) {
	var avail uint32
	r, _, err := procPeekNamedPipe.Call(uintptr(h), 0, 0, 0, uintptr(unsafe.Pointer(&avail)), 0)
	if r == 0 {
		return 0, err
	}
	return avail, nil
}

// Reads one chunk when PeekNamedPipe reports bytes, idle for the timeout otherwise, error on a broken pipe
func readChunkTimeout(f *os.File, buf []byte, timeout time.Duration) (int, bool, error) {
	h := windows.Handle(f.Fd())
	avail, err := peekNamedPipe(h)
	if err != nil {
		return 0, false, err
	}
	if avail == 0 {
		time.Sleep(timeout)
		return 0, false, nil
	}
	want := int(avail)
	if want > len(buf) {
		want = len(buf)
	}
	n, err := f.Read(buf[:want])
	if n > 0 {
		return n, true, nil
	}
	if err == nil {
		err = io.EOF
	}
	return 0, false, err
}

// Windows has no process groups to signal, the tree walk covers the children
func killProcessGroup(pid int) {}
