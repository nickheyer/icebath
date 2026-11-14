package auth

import (
	"os"
	"syscall"
)

func statIdentity(fi os.FileInfo) (ino, ctime int64) {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return 0, 0
	}
	return 0, d.CreationTime.Nanoseconds()
}
