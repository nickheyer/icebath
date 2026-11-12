package hardening

import "syscall"

var windowsCodes = map[syscall.Errno]string{
	1: "EISDIR", 2: "ENOENT", 3: "ENOENT", 4: "EMFILE", 5: "EPERM", 6: "EBADF", 8: "ENOMEM", 14: "ENOMEM", 15: "ENOENT",
	17: "EXDEV", 19: "EROFS", 32: "EBUSY", 33: "EBUSY", 39: "ENOSPC", 50: "ENOTSUP", 80: "EEXIST", 87: "EINVAL", 111: "ENAMETOOLONG",
	112: "ENOSPC", 123: "ENOENT", 126: "ENOENT", 142: "EBUSY", 145: "ENOTEMPTY", 170: "EBUSY", 183: "EEXIST", 206: "ENAMETOOLONG",
	267: "ENOTDIR", 998: "EACCES", 1314: "EPERM", 1920: "EACCES", 1921: "ELOOP", 4390: "ENOENT", 740: "EACCES", 121: "ETIMEDOUT",
}

// Node code libuv translates a Windows error number to
func uvCode(errno syscall.Errno) (string, bool) {
	code, ok := windowsCodes[errno]
	return code, ok
}
