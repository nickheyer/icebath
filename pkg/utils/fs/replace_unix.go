//go:build !windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"sync"
	"syscall"
)

// Permission bits a plain create gives a new file under the process umask
func newFileMode() (uint32, bool) { return 0o666 &^ processUmask(), true }

// Serializes reads of the umask by setting it, so two of them never leave it at the value one of them set
var umaskMu sync.Mutex

// The process umask read by setting it and putting it back, which narrows files other goroutines create in between, so only the fallback where nothing reports it
func swapUmask() uint32 {
	umaskMu.Lock()
	defer umaskMu.Unlock()
	current := syscall.Umask(0o077)
	syscall.Umask(current)
	return uint32(current)
}

// Whether a rename failed because another process holds the target, which only happens on Windows
func IsRenameContended(err error) bool { return false }

// Whether a rename failed because the target is on another device or busy, a failure a copy works around
func IsRenameCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EBUSY)
}

// Sets all twelve permission bits on the open temp file before it is published
func fchmodStaged(f *os.File, mode uint32) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var chmodErr error
	if err := conn.Control(func(fd uintptr) { chmodErr = syscall.Fchmod(int(fd), mode&0o7777) }); err != nil {
		return err
	}
	if chmodErr != nil {
		return &fs.PathError{Op: "chmod", Path: f.Name(), Err: chmodErr}
	}
	return nil
}
