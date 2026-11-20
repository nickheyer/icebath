//go:build !windows

package pyfs

import "golang.org/x/sys/unix"

// Junctions exist only on Windows
func isMountPoint(string) bool { return false }

// Sets the access and modification times of the link itself from the source link
func copySymlinkTimes(src, dst string) error {
	var st unix.Stat_t
	if err := unix.Lstat(src, &st); err != nil {
		return err
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, dst, []unix.Timespec{st.Atim, st.Mtim}, unix.AT_SYMLINK_NOFOLLOW)
}
