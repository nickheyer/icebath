//go:build !windows

package harness

import (
	"strings"

	"golang.org/x/sys/unix"
)

// Kernel name and release as uname reports them, empty when the call fails
func osVersion() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return strings.TrimSpace(cstring(u.Sysname[:]) + " " + cstring(u.Release[:]))
}

func cstring(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
