package file

import (
	"os"
	"syscall"
)

func fileStamp(path string, fi os.FileInfo) (uint64, uint64, int64, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, &os.PathError{Op: "stat", Path: path, Err: syscall.ENOTSUP}
	}
	return uint64(st.Dev), uint64(st.Ino), st.Ctim.Nano(), nil
}
