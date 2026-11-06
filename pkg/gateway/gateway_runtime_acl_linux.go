package gateway

import (
	"errors"
	"strings"

	"golang.org/x/sys/unix"
)

// Whether a POSIX access ACL is present on path or its absence cannot be established
func mayHaveAccessACL(path string) bool {
	size, err := unix.Llistxattr(path, nil)
	for err == nil {
		buf := make([]byte, size)
		var n int
		n, err = unix.Llistxattr(path, buf)
		if errors.Is(err, unix.ERANGE) {
			size, err = unix.Llistxattr(path, nil)
			continue
		}
		if err != nil {
			break
		}
		for _, name := range strings.Split(string(buf[:n]), "\x00") {
			if name == "system.posix_acl_access" {
				return true
			}
		}
		return false
	}
	return !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP)
}
