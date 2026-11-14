package catalog

import (
	"io/fs"
	"syscall"
)

func statIdentity(_ string, info fs.FileInfo) (inode uint64, changed int64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, info.ModTime().UnixNano()
	}
	return st.Ino, st.Ctimespec.Nano()
}
