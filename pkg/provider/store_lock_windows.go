//go:build windows

package provider

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
	procOpenProcess  = kernel32.NewProc("OpenProcess")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
	errLockViolation        = syscall.Errno(33)
	errIOPending            = syscall.Errno(997)
)

// kernelLock takes or releases a one byte exclusive range lock at offset zero
func kernelLock(f *os.File, acquire bool) error {
	var ol syscall.Overlapped
	h := uintptr(f.Fd())
	var r1 uintptr
	var err error
	if acquire {
		r1, _, err = procLockFileEx.Call(h, uintptr(lockfileExclusiveLock|lockfileFailImmediately), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	} else {
		r1, _, err = procUnlockFileEx.Call(h, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	}
	if r1 == 0 {
		return err
	}
	return nil
}

// isLockContention reports whether the range lock failure means another holder
func isLockContention(err error) bool {
	return errors.Is(err, errLockViolation) || errors.Is(err, errIOPending) || errors.Is(err, syscall.EACCES)
}

// processAlive reports whether a pid can be opened for limited query
func processAlive(pid int) bool {
	h, _, _ := procOpenProcess.Call(0x1000, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	syscall.CloseHandle(syscall.Handle(h))
	return true
}

func openLockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		f.WriteString(" ")
	}
	f.Seek(0, 0)
	return f, nil
}
