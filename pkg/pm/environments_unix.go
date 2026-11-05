//go:build unix

package pm

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// Whole filesystem flush a selection record waits for, the os sync call
var filesystemSync = func() error {
	syscall.Sync()
	return nil
}

// Machine name uname reports, the value Python platform machine returns
func platformMachine() string {
	var name unix.Utsname
	if unix.Uname(&name) != nil {
		return ""
	}
	return unix.ByteSliceToString(name.Machine[:])
}
