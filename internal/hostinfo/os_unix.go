//go:build linux || darwin

package hostinfo

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Identity fields uname reports for the running kernel
func unameOS() (OS, error) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return OS{}, fmt.Errorf("uname: %w", err)
	}
	return OS{
		System:  unix.ByteSliceToString(u.Sysname[:]),
		Release: unix.ByteSliceToString(u.Release[:]),
		Version: unix.ByteSliceToString(u.Version[:]),
		Machine: unix.ByteSliceToString(u.Machine[:]),
		Node:    unix.ByteSliceToString(u.Nodename[:]),
	}, nil
}
