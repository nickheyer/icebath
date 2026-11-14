package auth

import (
	"os"
	"syscall"
)

func statIdentity(fi os.FileInfo) (ino, ctime int64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return int64(st.Ino), syscall.TimespecToNsec(st.Ctim)
}
