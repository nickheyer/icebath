//go:build !windows

package home

import (
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Inode, mtime ns and ctime ns of entry, the ctime catching chown and chmod fixes that keep mtime
func entryFingerprint(entry string) ([]int64, bool) {
	var st unix.Stat_t
	if unix.Lstat(entry, &st) != nil {
		return nil, false
	}
	ino := new(big.Int).SetUint64(uint64(st.Ino))
	if !ino.IsInt64() {
		return nil, false
	}
	return []int64{ino.Int64(), st.Mtim.Nano(), st.Ctim.Nano()}, true
}

// Whether this user may now list and delete inside path by effective ids, true when it is gone
func accessibleNow(path string) bool {
	if _, err := os.Lstat(path); err != nil {
		return true
	}
	return unix.Faccessat(unix.AT_FDCWD, path, unix.R_OK|unix.W_OK|unix.X_OK, unix.AT_EACCESS) == nil
}

// Runs fn under an exclusive flock on the stuck lock file of root
func lockStuck(root string, fn func()) error {
	f, err := os.OpenFile(filepath.Join(root, StuckLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	fn()
	return nil
}

// Hard link count of a non following stat, zero when the platform does not report one
func linkCount(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 0
}
