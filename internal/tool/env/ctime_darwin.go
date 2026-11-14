package env

import (
	"io/fs"
	"syscall"
)

func changeTime(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ctimespec.Sec*1_000_000_000 + st.Ctimespec.Nsec
	}
	return 0
}
