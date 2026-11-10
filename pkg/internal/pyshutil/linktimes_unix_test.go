//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pyshutil

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Sets the access and modification time of a symlink itself
func setLinkTimes(t *testing.T, path string, at time.Time) {
	t.Helper()
	ts := []unix.Timespec{unix.NsecToTimespec(at.UnixNano()), unix.NsecToTimespec(at.UnixNano())}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
}
