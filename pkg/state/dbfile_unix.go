//go:build !windows

package state

import (
	"os"
	"syscall"
)

// Device and inode of an open file through fstat
func openFileIdentity(f *os.File) (FileIdentity, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return FileIdentity{}, err
	}
	return FileIdentity{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

// Device and inode of a raw descriptor through fstat
func fdIdentity(fd int) (FileIdentity, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return FileIdentity{}, err
	}
	return FileIdentity{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

// Size of a raw descriptor through fstat
func fdSize(fd int) (int64, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return 0, err
	}
	return st.Size, nil
}

// Device and inode of a path through stat, with the error stat raised
func statIdentityErr(path string) (FileIdentity, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return FileIdentity{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return FileIdentity{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

// One pread of up to length bytes at offset from a descriptor this process does not own
func rawPread(fd int, length int, offset int64) ([]byte, error) {
	buf := make([]byte, length)
	for {
		n, err := syscall.Pread(fd, buf, offset)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, &os.SyscallError{Syscall: "pread", Err: err}
		}
		return buf[:n], nil
	}
}

// Non blocking exclusive flock of the quarantine lock file
func quarantineTryLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// Releases the quarantine flock
func quarantineUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
