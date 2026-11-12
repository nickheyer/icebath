//go:build !windows

package hardening

import "syscall"

var unixCodes = map[syscall.Errno]string{
	syscall.E2BIG: "E2BIG", syscall.EACCES: "EACCES", syscall.EAGAIN: "EAGAIN", syscall.EBADF: "EBADF", syscall.EBUSY: "EBUSY",
	syscall.ECANCELED: "ECANCELED", syscall.EEXIST: "EEXIST", syscall.EFAULT: "EFAULT", syscall.EFBIG: "EFBIG", syscall.EINTR: "EINTR",
	syscall.EINVAL: "EINVAL", syscall.EIO: "EIO", syscall.EISDIR: "EISDIR", syscall.ELOOP: "ELOOP", syscall.EMFILE: "EMFILE",
	syscall.EMLINK: "EMLINK", syscall.ENAMETOOLONG: "ENAMETOOLONG", syscall.ENFILE: "ENFILE", syscall.ENODEV: "ENODEV",
	syscall.ENOENT: "ENOENT", syscall.ENOMEM: "ENOMEM", syscall.ENOSPC: "ENOSPC", syscall.ENOSYS: "ENOSYS", syscall.ENOTDIR: "ENOTDIR",
	syscall.ENOTEMPTY: "ENOTEMPTY", syscall.ENOTSUP: "ENOTSUP", syscall.ENXIO: "ENXIO", syscall.EOVERFLOW: "EOVERFLOW",
	syscall.EPERM: "EPERM", syscall.EPIPE: "EPIPE", syscall.EROFS: "EROFS", syscall.ESPIPE: "ESPIPE", syscall.ETIMEDOUT: "ETIMEDOUT",
	syscall.ETXTBSY: "ETXTBSY", syscall.EXDEV: "EXDEV",
}

// Node code of a POSIX errno
func uvCode(errno syscall.Errno) (string, bool) {
	code, ok := unixCodes[errno]
	return code, ok
}
