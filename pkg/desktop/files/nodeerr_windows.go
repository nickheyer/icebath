//go:build windows

package files

import "syscall"

// Win32 codes to libuv names as uv_translate_sys_error maps them
var winErrorNames = map[syscall.Errno]string{
	2: "ENOENT", 3: "ENOENT", 15: "ENOENT", 21: "EAGAIN", 53: "ENOENT", 67: "ENOENT", 123: "ENOENT", 161: "ENOENT",
	5: "EPERM", 1314: "EPERM", 998: "EACCES", 4: "EMFILE", 8: "ENOMEM", 14: "ENOMEM", 19: "EROFS",
	32: "EBUSY", 33: "EBUSY", 170: "EBUSY", 80: "EEXIST", 183: "EEXIST", 87: "EINVAL", 112: "ENOSPC", 39: "ENOSPC",
	145: "ENOTEMPTY", 17: "EXDEV", 206: "ENAMETOOLONG", 1921: "ELOOP", 267: "ENOTDIR", 50: "ENOTSUP",
	109: "EPIPE", 232: "EPIPE", 1460: "ETIMEDOUT", 995: "ECANCELED", 1117: "EIO", 1784: "EFAULT", 4390: "EINVAL",
}

// libuv code name of a Win32 error
func uvCode(errno syscall.Errno) (string, bool) {
	name, ok := winErrorNames[errno]
	return name, ok
}
