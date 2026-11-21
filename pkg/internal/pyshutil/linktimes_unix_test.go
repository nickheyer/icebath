//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pyshutil

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Sets the access and modification times of a symlink itself
func setLinkTimes(t *testing.T, path string, atime, mtime time.Time) {
	t.Helper()
	ts := []unix.Timespec{unix.NsecToTimespec(atime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
}
