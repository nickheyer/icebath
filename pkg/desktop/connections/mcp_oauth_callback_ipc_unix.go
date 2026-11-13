//go:build !windows

package connections

import "syscall"

// Node code names of the errnos a loopback listen fails with
var listenErrnoCodes = map[syscall.Errno]string{
	syscall.EADDRINUSE: "EADDRINUSE", syscall.EADDRNOTAVAIL: "EADDRNOTAVAIL", syscall.EACCES: "EACCES",
	syscall.EMFILE: "EMFILE", syscall.ENFILE: "ENFILE", syscall.ENOBUFS: "ENOBUFS", syscall.EPERM: "EPERM",
	syscall.EAFNOSUPPORT: "EAFNOSUPPORT", syscall.EINVAL: "EINVAL",
}
