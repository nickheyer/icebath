//go:build !windows

package updaterecord

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const noFollow = syscall.O_NOFOLLOW

// Opens and flocks the sidecar exclusively, read only when the sidecar belongs to another user
func mutexAcquire(path string, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if errors.Is(err, fs.ErrPermission) {
		f, err = os.Open(path)
	}
	if err != nil {
		return nil, err
	}
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			f.Close()
			return nil, &os.PathError{Op: "flock", Path: path, Err: err}
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, &MarkerBusy{Path: path}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Takes the exclusive flock without waiting, false while another description holds it
func tryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return false, &os.PathError{Op: "flock", Path: f.Name(), Err: err}
}

// Shared flock probe dropped when the caller closes f, false on a holder
func trySharedLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	pauseAt("shared-probe")
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}

func unlockBytes(f *os.File, offset int64, count uint32) {}

func takeLease(f *os.File) (int64, bool) { return 0, false }

// Inheritable descriptor of this process that already holds the flock on path
func inheritedLockFD(path string) (int, bool) {
	var target unix.Stat_t
	if err := unix.Stat(path, &target); err != nil {
		return -1, false
	}
	dir := "/proc/self/fd"
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		dir = "/dev/fd"
	}
	names, err := readDirNames(dir)
	if err != nil {
		return -1, false
	}
	for _, name := range names {
		if !intLineRE.MatchString(name) {
			continue
		}
		fd, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC != 0 {
			continue
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || st.Dev != target.Dev || st.Ino != target.Ino {
			continue
		}
		if unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
			continue
		}
		return fd, true
	}
	return -1, false
}

// Hard link count of an open file
func linkCount(f *os.File) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return uint64(st.Nlink), nil
}

// Binds an update tree child to the kill on close job, POSIX children inherit the lock descriptor instead
func BindChildToUpdateTree(pid int) error { return nil }

// Resumes a suspended update child, POSIX children never start suspended
func ResumeSuspendedChild(process *os.Process) error { return nil }
