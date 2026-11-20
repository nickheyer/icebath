//go:build linux && !386 && !arm

package containerinit

import "golang.org/x/sys/unix"

// Per thread credential syscalls taking 32 bit ids
const (
	sysSetuid = unix.SYS_SETUID
	sysSetgid = unix.SYS_SETGID
)
