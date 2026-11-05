//go:build windows

package state

import (
	"os"

	"golang.org/x/sys/windows"
)

// Volume serial number and file index of a handle
func handleIdentity(h windows.Handle) (FileIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return FileIdentity{}, err
	}
	return FileIdentity{Dev: uint64(info.VolumeSerialNumber), Ino: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)}, nil
}

// Volume serial number and file index of an open file
func openFileIdentity(f *os.File) (FileIdentity, error) {
	return handleIdentity(windows.Handle(f.Fd()))
}

// Volume serial number and file index of a raw handle
func fdIdentity(fd int) (FileIdentity, error) {
	return handleIdentity(windows.Handle(fd))
}

// Size of a raw handle
func fdSize(fd int) (int64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(fd), &info); err != nil {
		return 0, err
	}
	return int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow), nil
}

// Volume serial number and file index of a path, with the error opening it raised
func statIdentityErr(path string) (FileIdentity, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileIdentity{}, err
	}
	defer f.Close()
	return openFileIdentity(f)
}

// One positioned read of up to length bytes at offset from a raw handle
func rawPread(fd int, length int, offset int64) ([]byte, error) {
	buf := make([]byte, length)
	ol := &windows.Overlapped{Offset: uint32(offset), OffsetHigh: uint32(offset >> 32)}
	var n uint32
	if err := windows.ReadFile(windows.Handle(fd), buf, &n, ol); err != nil && err != windows.ERROR_HANDLE_EOF {
		return nil, &os.SyscallError{Syscall: "ReadFile", Err: err}
	}
	return buf[:n], nil
}

// Non blocking lock of the first byte of the quarantine lock file, the msvcrt LK_NBLCK contract
func quarantineTryLock(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
}

// Unlocks the first byte of the quarantine lock file
func quarantineUnlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
