//go:build !windows

package cron

import "golang.org/x/sys/unix"

// Write access the way os.access W_OK reports it for the real user
func writable(path string) bool { return unix.Access(path, unix.W_OK) == nil }

// Root reads every free block through statvfs, others the blocks available to unprivileged users
func freeBytes(path string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, false
	}
	if ownership.euid() == 0 {
		return uint64(st.Bfree) * fragmentSize(&st), true
	}
	return uint64(st.Bavail) * fragmentSize(&st), true
}
