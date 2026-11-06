//go:build unix

package pyplatform

import "golang.org/x/sys/unix"

func uname() (string, string, bool) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "", "", false
	}
	return unix.ByteSliceToString(u.Sysname[:]), unix.ByteSliceToString(u.Machine[:]), true
}

func system() string {
	if sysname, _, ok := uname(); ok {
		return sysname
	}
	return goosSystem()
}

func machine() string {
	_, m, _ := uname()
	return m
}
