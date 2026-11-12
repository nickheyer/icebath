//go:build linux && (386 || arm)

package containerinit

import "golang.org/x/sys/unix"

// Per thread credential syscalls taking 32 bit ids on the 32 bit ABIs
const (
	sysSetuid = unix.SYS_SETUID32
	sysSetgid = unix.SYS_SETGID32
)
