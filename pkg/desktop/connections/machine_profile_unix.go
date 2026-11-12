//go:build !windows

package connections

import "golang.org/x/sys/unix"

// Kernel release from uname, the value Node os.release reports
func osRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}
